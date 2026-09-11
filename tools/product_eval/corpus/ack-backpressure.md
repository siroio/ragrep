# Acknowledgement and Backpressure

HarborQueue uses explicit acknowledgement. A consumer must acknowledge only after the side effect is durable; acknowledging on receipt can lose work when the process crashes. This rule applies to every queue.

The broker applies backpressure when in-flight messages exceed 2,000 per consumer or disk usage reaches 80 percent. Publishers then receive `HQ_BACKPRESSURE` and should retry with exponential backoff.

A slow consumer should lower its prefetch value before adding more workers. The acknowledgement endpoint is idempotent for the same message and lease, so a client may safely retry a timed-out acknowledgement.
