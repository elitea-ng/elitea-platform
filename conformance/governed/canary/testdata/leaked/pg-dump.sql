-- deliberate leak: canary self-test fixture
COPY elitea_runtime.execution_security_contexts (execution_id, context_bytes) FROM stdin;
exec-1	\x0a1b1234677663616e6172792d73656c66746573742d3566306331643265336134623563366437653866393031323334353637383930
\.
