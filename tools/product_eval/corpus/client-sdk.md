# Client SDK Guidance

The SDK exposes publish, receive, renew, acknowledge, and retry operations. A receive call may return the same message after a lease expires, so application code must treat delivery as at-least-once. Keep the SDK version in deployment notes.

Set a receive timeout shorter than the lease timeout and renew while processing long work. Shutdown should stop receiving, finish bounded work, and acknowledge only completed messages.

The SDK reports `HQ_BACKPRESSURE` separately from transport failures. Backpressure should slow producers; transport failures should reconnect with jitter so many clients do not retry together.
