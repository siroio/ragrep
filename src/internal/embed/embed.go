package embed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/eliben/go-sentencepiece"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	// must match onnxruntime_go's compiled ORT_API_VERSION (v1.27.0 = API 24);
	// also the last ORT version Microsoft shipped a DirectML build for.
	ortVersion = "1.24.4"
	dmlVersion = "1.15.4" // Microsoft.AI.DirectML version the ORT DirectML nuspec pins
	// Pinned to a commit sha (not "main") so the download can't silently
	// change contents out from under a cached, unverified file.
	repoBase        = "https://huggingface.co/onnx-community/embeddinggemma-300m-ONNX/resolve/5090578d9565bb06545b4552f76e6bc2c93e4a66/"
	spmURL          = repoBase + "tokenizer.model"
	spmFile         = "tokenizer.model"
	modelSHA256     = "172efde319fe1542dc41f31be6154910b05b78f7a861c265c4600eec906bd6d8"
	modelDataSHA256 = "705626e28e4c23c82ade34566b4197d97f534c12275fa406dfb71e9937d388c0"
	spmSHA256       = "1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c"

	maxTokens = 1022 // ponytail: truncate long paragraphs (context 2048); FTS still covers the tail

	bosID = 2
	eosID = 1
)

// useDirectML: Windows runs inference on the GPU via the DirectML execution
// provider. The int8-quantized model's ops aren't GPU-executable there, so
// Windows uses the fp32 model (NOT fp16: Gemma activations overflow fp16's
// range, and DML — unlike the CPU EP, which upcasts internally — executes
// true fp16, yielding all-NaN embeddings). Other platforms keep the smaller
// quantized model on CPU. Changing modelFile changes embedding values — bump
// store.HashContent's version prefix so existing indexes re-embed.
var useDirectML = runtime.GOOS == "windows"

func modelName() string {
	if useDirectML {
		return "model.onnx"
	}
	return "model_quantized.onnx"
}

// ModelCached reports whether every asset New needs is already cached in dir.
func ModelCached(dir string) bool {
	assets, err := ortAssetsFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return false
	}
	return missingAsset(dir, assets) == ""
}

// RuntimeIdentity returns the pinned inference runtime used by the asset set.
func RuntimeIdentity() string {
	if runtime.GOOS == "windows" {
		return "onnxruntime@" + ortVersion + "+directml@" + dmlVersion
	}
	return "onnxruntime@" + ortVersion
}

var (
	modelFile     = modelName()
	modelDataFile = modelFile + "_data" // external weights; name is baked into modelFile's graph, must not be renamed
	modelURL      = repoBase + "onnx/" + modelFile
	modelDataURL  = repoBase + "onnx/" + modelDataFile
)

var assetHTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second}}

// embedDim is the embeddinggemma-300m output dimension (matches the
// sentence_embedding graph output; store's vec0 schema is pinned to the same
// value independently since the two packages don't import each other).
const embedDim = 768

type ortAsset struct{ url, inner, lib, sha256, archiveSHA256 string }

func nupkgURL(pkg, ver string) string {
	return "https://api.nuget.org/v3-flatcontainer/" + pkg + "/" + ver + "/" + pkg + "." + ver + ".nupkg"
}

// ortAssetsFor returns the archives to fetch and the shared libraries to
// extract from them. The first entry is always the ONNX runtime library
// itself. Windows uses the DirectML-enabled runtime from NuGet (the GitHub
// release builds are CPU-only) plus its DirectML.dll dependency; other
// platforms use the CPU build from GitHub releases.
func ortAssetsFor(goos, goarch string) ([]ortAsset, error) {
	base := "https://github.com/microsoft/onnxruntime/releases/download/v" + ortVersion + "/"
	dmlAssets := func(rid, dmlArch string) []ortAsset {
		return []ortAsset{
			{nupkgURL("microsoft.ml.onnxruntime.directml", ortVersion),
				"runtimes/" + rid + "/native/onnxruntime.dll", "onnxruntime.dll", map[string]string{"win-x64": "e7eedec6a6f26dc39dc948276a75ef6d2bee3fff944d874ceed0bbd3b97bff40", "win-arm64": "939fa0be435d253eadd9e394ee94ad6b6ac1878c83b64118bb4bda669eb0e28c"}[rid], "57e9f11b73437bef7a309496135d4c1f96b1a8e9ddba60013fa27bfc1d788681"},
			{nupkgURL("microsoft.ai.directml", dmlVersion),
				"bin/" + dmlArch + "-win/DirectML.dll", "DirectML.dll", map[string]string{"x64": "9c9e6d822561c6c41b90e6994b3e8857cf1d66dbfb1e0c4c799c7c89b4e92da1", "arm64": "77b0db83ff903f2323f5caf538499d75af6038bbea23b7959f7d232d9a4ab9d4"}[dmlArch], "4e7cb7ddce8cf837a7a75dc029209b520ca0101470fcdf275c1f49736a3615b9"},
		}
	}
	m := map[string][]ortAsset{
		"windows/amd64": dmlAssets("win-x64", "x64"),
		"windows/arm64": dmlAssets("win-arm64", "arm64"),
		"linux/amd64": {{base + "onnxruntime-linux-x64-" + ortVersion + ".tgz",
			"onnxruntime-linux-x64-" + ortVersion + "/lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so", "d132535d051344ff5c64c9c200004150559049a81ed330eb4422c1962fb6b7e4", "3a211fbea252c1e66290658f1b735b772056149f28321e71c308942cdb54b747"}},
		"darwin/arm64": {{base + "onnxruntime-osx-arm64-" + ortVersion + ".tgz",
			"onnxruntime-osx-arm64-" + ortVersion + "/lib/libonnxruntime." + ortVersion + ".dylib", "libonnxruntime.dylib", "872533f130f1839a5bc01788ddb4f75c83a189763441ba1178788ed965449289", "93787795f47e1eee369182e43ed51b9e5da0878ab0346aecf4258979b8bba989"}},
	}
	a, ok := m[goos+"/"+goarch]
	if !ok {
		return nil, fmt.Errorf("unsupported platform %s/%s", goos, goarch)
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
	dir := filepath.Join(base, "ragrep")
	return dir, os.MkdirAll(dir, 0o755)
}

func download(url, dest string) error {
	return downloadContext(context.Background(), url, dest, "", 2*time.Minute)
}

func downloadContext(ctx context.Context, url, dest, wantSHA256 string, timeout time.Duration) error {
	if ok, err := verifiedFileContext(ctx, dest, wantSHA256); err != nil {
		return err
	} else if ok {
		return nil
	}
	if wantSHA256 != "" {
		_ = os.Remove(dest)
	}
	fmt.Fprintf(os.Stderr, "downloading %s\n", url)
	if err := ctx.Err(); err != nil {
		return err
	}
	requestCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := assetHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ok, err := verifiedFileContext(ctx, tmp, wantSHA256); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("checksum mismatch for %s", filepath.Base(dest))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if ok, verifyErr := verifiedFile(dest, wantSHA256); verifyErr == nil && ok {
			return nil
		}
		return err
	}
	return nil
}

func verifiedFile(path, wantSHA256 string) (bool, error) {
	return verifiedFileContext(context.Background(), path, wantSHA256)
}

func verifiedFileContext(ctx context.Context, path, wantSHA256 string) (bool, error) {
	if wantSHA256 == "" {
		_, err := os.Stat(path)
		return err == nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, readErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA256, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

func expectedAssetHash(name string) string {
	switch name {
	case modelFile:
		if modelFile == "model.onnx" {
			return "ea91fd315a7c152d427d231746f0f811a1ac93beaba656abfdf2b24e091265e4"
		}
		return modelSHA256
	case modelDataFile:
		if modelFile == "model.onnx" {
			return "ef835ae565d8695236652475903078e8ed794c7c35faf1164d78ec3238e8a88d"
		}
		return modelDataSHA256
	case spmFile:
		return spmSHA256
	default:
		return ""
	}
}

// libDir returns the versioned subdirectory holding the extracted runtime
// libraries. Versioning the path (not just the filename) means bumping
// ortVersion can never silently reuse a stale library extracted from an
// older release — and DirectML.dll keeps its exact name, which the Windows
// loader requires.
func libDir(dir string) string {
	return filepath.Join(dir, "ort-"+ortVersion)
}

// extractOrtLib downloads the onnxruntime release archive and extracts the
// shared library named by asset.inner into libDir(dir) as asset.lib.
func extractOrtLib(ctx context.Context, dir string, asset ortAsset) error {
	if err := os.MkdirAll(libDir(dir), 0o755); err != nil {
		return err
	}
	dest := filepath.Join(libDir(dir), asset.lib)
	if ok, err := verifiedFileContext(ctx, dest, asset.sha256); err != nil {
		return err
	} else if ok {
		return nil
	}
	archive := filepath.Join(dir, filepath.Base(asset.url))
	if err := downloadContext(ctx, asset.url, archive, asset.archiveSHA256, 2*time.Minute); err != nil {
		return err
	}
	writeLib := func(r io.Reader) error {
		f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+"-*.tmp")
		if err != nil {
			return err
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		if _, err := io.Copy(f, contextReader{ctx, r}); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if ok, err := verifiedFileContext(ctx, tmp, asset.sha256); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("checksum mismatch for %s", asset.lib)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			if ok, verifyErr := verifiedFile(dest, asset.sha256); verifyErr == nil && ok {
				return nil
			}
			return err
		}
		return nil
	}

	if ext := filepath.Ext(archive); ext == ".zip" || ext == ".nupkg" {
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
	return EnsureAssetsContext(context.Background(), dir)
}

func EnsureAssetsContext(ctx context.Context, dir string) error {
	assets, err := ortAssetsFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if err := extractOrtLib(ctx, dir, asset); err != nil {
			return err
		}
	}
	if err := downloadContext(ctx, modelURL, filepath.Join(dir, modelFile), expectedAssetHash(modelFile), 2*time.Minute); err != nil {
		return err
	}
	if err := downloadContext(ctx, modelDataURL, filepath.Join(dir, modelDataFile), expectedAssetHash(modelDataFile), 30*time.Minute); err != nil {
		return err
	}
	return downloadContext(ctx, spmURL, filepath.Join(dir, spmFile), expectedAssetHash(spmFile), 2*time.Minute)
}

type Embedder struct {
	proc *sentencepiece.Processor
	sess *ort.DynamicAdvancedSession
}

// missingAsset returns the name of the first required asset file not found
// in dir, or "" if all are present. Shared by New (to error out) and
// tests (to decide skip vs. run) so the two checks can't drift apart.
func missingAsset(dir string, assets []ortAsset) string {
	for _, f := range []string{modelFile, modelDataFile, spmFile} {
		ok, err := verifiedFile(filepath.Join(dir, f), expectedAssetHash(f))
		if err != nil || !ok {
			return f
		}
	}
	for _, a := range assets {
		ok, err := verifiedFileContext(context.Background(), filepath.Join(libDir(dir), a.lib), a.sha256)
		if err != nil || !ok {
			return a.lib
		}
	}
	return ""
}

// New creates an Embedder using the ONNX runtime, model, and tokenizer
// cached in dir (see EnsureAssets).
func New(dir string) (*Embedder, error) {
	assets, err := ortAssetsFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if f := missingAsset(dir, assets); f != "" {
		return nil, fmt.Errorf("missing %s: run 'ragrep init' first", f)
	}
	ort.SetSharedLibraryPath(filepath.Join(libDir(dir), assets[0].lib))
	if useDirectML {
		// onnxruntime.dll resolves DirectML.dll through the standard DLL
		// search, which doesn't include the lib dir; PATH is searched last.
		os.Setenv("PATH", libDir(dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, err
	}
	proc, err := sentencepiece.NewProcessorFromPath(filepath.Join(dir, spmFile))
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	sess, err := newSession(filepath.Join(dir, modelFile))
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	return &Embedder{proc: proc, sess: sess}, nil
}

func newSession(model string) (*ort.DynamicAdvancedSession, error) {
	in := []string{"input_ids", "attention_mask"}
	out := []string{"sentence_embedding"}
	if useDirectML {
		sess, err := newDMLSession(model, in, out)
		if err == nil {
			return sess, nil
		}
		fmt.Fprintf(os.Stderr, "ragrep: DirectML unavailable (%v); falling back to CPU\n", err)
	}
	return ort.NewDynamicAdvancedSession(model, in, out, nil)
}

func newDMLSession(model string, in, out []string) (*ort.DynamicAdvancedSession, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	// The DirectML EP requires memory patterns to be disabled.
	if err := opts.SetMemPattern(false); err != nil {
		return nil, err
	}
	// All graph nodes run on the GPU; ORT's default core-count intra-op pool
	// would only spin-wait and burn CPU alongside it.
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, err
	}
	if err := opts.AppendExecutionProviderDirectML(0); err != nil {
		return nil, err
	}
	return ort.NewDynamicAdvancedSession(model, in, out, opts)
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
	// DirectML JIT-compiles kernels per input shape (~100ms per new sequence
	// length), so varying-length paragraphs would recompile on almost every
	// call. Pad to a few fixed bucket lengths instead; the zeroed attention
	// mask makes the in-graph pooling ignore the padding.
	padded := n
	if useDirectML {
		for _, b := range []int{64, 128, 256, 512, maxTokens + 2} {
			if n <= b {
				padded = b
				break
			}
		}
	}
	ids = append(ids, make([]int64, padded-n)...)
	mask := make([]int64, padded)
	for i := range mask[:n] {
		mask[i] = 1
	}

	idT, err := ort.NewTensor(ort.NewShape(1, int64(padded)), ids)
	if err != nil {
		return nil, err
	}
	defer idT.Destroy()
	maskT, err := ort.NewTensor(ort.NewShape(1, int64(padded)), mask)
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
