# Code execution acceptance fixture

Create a pipeline through the deployed UI using `code-multilanguage-state.yaml`.
Send the adjacent input JSON through both its test chat and persistent chat.
Compare the final JSON with `code-multilanguage-state-expected.json`.
Reload the persistent chat and verify that one final report remains.
Verify four completed supervisor receipts with distinct runtime identities.

Send `[]` to the same pipeline. Verify a Code failure and one failed Python receipt.
Verify that no downstream Code job starts for this failed run.
Send the valid input again. Verify that the next execution completes independently.

Use the same fixture for Docker and Kubernetes acceptance.
This fixture uses actual language runtimes and does not invoke a model.
It verifies state transfer, sorting, totals, and failure propagation, not load capacity.
