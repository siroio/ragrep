# Rolling Upgrade

Upgrade one broker at a time, keeping quorum available. Before draining a broker, confirm that replication lag is below 2 seconds and that another broker is the current leader. Record the broker version before the drain.

Clients tolerate a broker restart through reconnect and retry. Do not change wire protocol and authentication settings in the same rollout; separate those changes so a failure has one clear cause.

After each broker returns, check health, replication lag, publish latency, and consumer acknowledgement latency. Stop the rollout if any two consecutive checks exceed the baseline by 30 percent.
