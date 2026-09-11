# 認証とリース

クライアント認証には短命の `hq_token` を使い、producer と consumer に別の権限を与える。consumer はキューの読み取りと acknowledgement だけを持ち、管理 API の権限を持たない。権限は必要最小限にする。

メッセージを取得すると lease が発行され、既定の lease timeout は 45 秒である。処理が長い場合は期限の半分を過ぎる前に `renew` を送り、完了後に acknowledgement を送る。

lease 失効後の acknowledgement は受理されず、メッセージは再配信される。重複処理を安全にするため、業務側は `message_id` を保存して冪等性を確保する。
