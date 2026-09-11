# Retry and Dead Letter

A failed handler returns a retryable or permanent error. Retryable failures use delays of 5, 30, and 300 seconds, with at most three attempts before dead-lettering. Delays are measured from the failed delivery.

Permanent validation errors go directly to the dead-letter queue and should include a reason code. Operators may replay a dead-letter message only after correcting the consumer or payload.

Replay creates a new delivery attempt but retains the original `message_id`. Consumers must remain idempotent because a timeout can cause the same message to be delivered again.
