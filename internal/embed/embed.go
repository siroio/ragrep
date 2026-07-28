package embed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/eliben/go-sentencepiece"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	ortVersion = "1.26.0" // must match onnxruntime_go's compiled ORT_API_VERSION
	// Pinned to a commit sha (not "main") so the download can't silently
	// change contents out from under a cached, unverified file.
	repoBase     = "https://huggingface.co/onnx-community/embeddinggemma-300m-ONNX/resolve/5090578d9565bb06545b4552f76e6bc2c93e4a66/"
	modelURL     = repoBase + "onnx/model_quantized.onnx"
	modelDataURL = repoBase + "onnx/model_quantized.onnx_data"
	spmURL       = repoBase + "tokenizer.model"
	maxTokens    = 1022 // ponytail: truncate long paragraphs (context 2048); FTS still covers the tail

	modelFile     = "model_quantized.onnx"
	modelDataFile = "model_quantized.onnx_data" // external weights; name is baked into modelFile's graph, must not be renamed
	spmFile       = "tokenizer.model"

	bosID = 2
	eosID = 1
)

// embedDim is the embeddinggemma-300m output dimension (matches the
// sentence_embedding graph output; store's vec0 schema is pinned to the same
// value independently since the two packages don't import each other).
const embedDim = 768

type ortAsset struct{ url, inner, lib string }

func ortAssetFor(goos, goarch string) (ortAsset, error) {
	base := "https://github.com/microsoft/onnxruntime/releases/download/v" + ortVersion + "/"
	m := map[string]ortAsset{
		"windows/amd64": {base + "onnxruntime-win-x64-" + ortVersion + ".zip",
			"onnxruntime-win-x64-" + ortVersion + "/lib/onnxruntime.dll", "onnxruntime.dll"},
		"windows/arm64": {base + "onnxruntime-win-arm64-" + ortVersion + ".zip",
			"onnxruntime-win-arm64-" + ortVersion + "/lib/onnxruntime.dll", "onnxruntime.dll"},
		"linux/amd64": {base + "onnxruntime-linux-x64-" + ortVersion + ".tgz",
			"onnxruntime-linux-x64-" + ortVersion + "/lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so"},
		"darwin/arm64": {base + "onnxruntime-osx-arm64-" + ortVersion + ".tgz",
			"onnxruntime-osx-arm64-" + ortVersion + "/lib/libonnxruntime." + ortVersion + ".dylib", "libonnxruntime.dylib"},
	}
	a, ok := m[goos+"/"+goarch]
	if !ok {
		return ortAsset{}, fmt.Errorf("unsupported platform %s/%s", goos, goarch)
	}
	return a, nil
}

// CacheDir returns (creating if needed) the per-user cache directory where
// model/runtime assets are stored.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "rag")
	return dir, os.MkdirAll(dir, 0o755)
}

func download(url, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "downloading %s\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}

// extractOrtLib downloads the onnxruntime release archive and extracts the
// shared library named by asset.inner into dir as asset.lib.
func extractOrtLib(dir string, asset ortAsset) error {
	dest := filepath.Join(dir, asset.lib)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	archive := filepath.Join(dir, filepath.Base(asset.url))
	if err := download(asset.url, archive); err != nil {
		return err
	}
	defer os.Remove(archive)

	writeLib := func(r io.Reader) error {
		tmp := dest + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		f.Close()
		return os.Rename(tmp, dest)
	}

	if filepath.Ext(archive) == ".zip" {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, zf := range zr.File {
			if zf.Name == asset.inner {
				r, err := zf.Open()
				if err != nil {
					return err
				}
				defer r.Close()
				return writeLib(r)
			}
		}
	} else { // .tgz
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if hdr.Name == asset.inner {
				return writeLib(tr)
			}
		}
	}
	return fmt.Errorf("%s not found in %s", asset.inner, archive)
}

// EnsureAssets downloads (if not already cached in dir) the ONNX runtime
// shared library, the embedding model, and the tokenizer.
func EnsureAssets(dir string) error {
	asset, err := ortAssetFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := extractOrtLib(dir, asset); err != nil {
		return err
	}
	if err := download(modelURL, filepath.Join(dir, modelFile)); err != nil {
		return err
	}
	if err := download(modelDataURL, filepath.Join(dir, modelDataFile)); err != nil {
		return err
	}
	return download(spmURL, filepath.Join(dir, spmFile))
}

type Embedder struct {
	proc *sentencepiece.Processor
	sess *ort.DynamicAdvancedSession
}

// missingAsset returns the name of the first required asset file not found
// in dir, or "" if all are present. Shared by New (to error out) and
// tests (to decide skip vs. run) so the two checks can't drift apart.
func missingAsset(dir string, asset ortAsset) string {
	for _, f := range []string{asset.lib, modelFile, modelDataFile, spmFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return f
		}
	}
	return ""
}

// New creates an Embedder using the ONNX runtime, model, and tokenizer
// cached in dir (see EnsureAssets).
func New(dir string) (*Embedder, error) {
	asset, err := ortAssetFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if f := missingAsset(dir, asset); f != "" {
		return nil, fmt.Errorf("missing %s: run 'rag init' first", f)
	}
	ort.SetSharedLibraryPath(filepath.Join(dir, asset.lib))
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, err
	}
	proc, err := sentencepiece.NewProcessorFromPath(filepath.Join(dir, spmFile))
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	sess, err := ort.NewDynamicAdvancedSession(filepath.Join(dir, modelFile),
		[]string{"input_ids", "attention_mask"}, []string{"sentence_embedding"}, nil)
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	return &Embedder{proc: proc, sess: sess}, nil
}

func (e *Embedder) Close() {
	e.sess.Destroy()
	ort.DestroyEnvironment()
}

func (e *Embedder) Embed(text string) ([]float32, error) {
	toks := e.proc.Encode(text)
	ids := []int64{bosID}
	for _, t := range toks {
		if len(ids) > maxTokens {
			break // ponytail: truncate long paragraphs; FTS still covers the tail
		}
		ids = append(ids, int64(t.ID))
	}
	ids = append(ids, eosID)
	n := len(ids)
	mask := make([]int64, n)
	for i := range mask {
		mask[i] = 1
	}

	idT, err := ort.NewTensor(ort.NewShape(1, int64(n)), ids)
	if err != nil {
		return nil, err
	}
	defer idT.Destroy()
	maskT, err := ort.NewTensor(ort.NewShape(1, int64(n)), mask)
	if err != nil {
		return nil, err
	}
	defer maskT.Destroy()

	outputs := []ort.Value{nil}
	if err := e.sess.Run([]ort.Value{idT, maskT}, outputs); err != nil {
		return nil, err
	}
	out, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output tensor type %T", outputs[0])
	}
	defer out.Destroy()

	// The exported graph's "sentence_embedding" output is already mean-pooled
	// over tokens ([1, embedDim]); just L2-normalize it.
	data := out.GetData()
	emb := make([]float32, embedDim)
	copy(emb, data)

	var norm float64
	for _, x := range emb {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return emb, nil
	}
	for j := range emb {
		emb[j] = float32(float64(emb[j]) / norm)
	}
	return emb, nil
}
