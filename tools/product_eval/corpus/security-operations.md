# セキュリティ運用

`hq_token` は 24 時間の有効期間が終わる前にローテーションし、全クライアントの切替後に古い token を revoke する。token ローテーションに broker 再起動は不要で、担当者と完了時刻を記録する。

ネットワークアクセスは broker listener と metrics endpoint に制限する。metrics endpoint に payload データはないが、信頼済み監視ネットワーク外ではサービス認証を要求する。

監査記録は actor、action、queue、result を 90 日間保持する。チケットへ payload や token をコピーせず、request id と audit record id を使う。
