# Persistence and Recovery

HarborQueue persists each accepted message before publish returns success. A broker restart therefore preserves accepted messages, while a client timeout may still cause the client to retry the publish.

Recovery replays unacknowledged messages after the broker restores its log. Consumers must tolerate duplicate delivery and should store the message id with the business result.

Operators verify the recovered log position, queue depth, and oldest message age before reopening traffic. A healthy broker is not sufficient if the consumer group is still behind.
