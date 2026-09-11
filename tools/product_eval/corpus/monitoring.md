# Monitoring and Alerts

Monitor queue depth, oldest message age, publish rate, acknowledgement rate, retry rate, and dead-letter rate. A depth alert without message age is insufficient because a large but freshly drained queue may be healthy. Record alert windows in UTC. Keep dashboard labels stable during upgrades.

Page the on-call when oldest message age exceeds 10 minutes for five minutes, or when dead-letter rate exceeds 2 percent for ten minutes. A warning at 5 minutes gives operators time to react.

Dashboards should separate each tenant and queue. Correlate broker CPU, disk usage, consumer latency, and `HQ_BACKPRESSURE` responses before changing capacity.
