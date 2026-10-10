# Fork maintenance

This fork carries the mcp2cli proof of concept used by Alex Darbyshire's kagent estate. Maintain upstream freshness while preserving the fork's command adapter, scoped credentials, runtime continuity, shutdown behavior and artifact handling.

## Branches

- Upstream source: `kagent-dev/kagent`, branch `main`.
- Fork mirror: `alexdarbyshire/kagent`, branch `main`. Keep it aligned with upstream through fast-forward updates when possible.
- Fork integration: `feature/mcp-cli`. Topic changes and upstream sync PRs target this branch.

Preserve published integration history. Merge upstream through a separate sync branch; never replace `feature/mcp-cli` with upstream or force-push it. Source synchronization does not deploy the estate.

## Upstream sync

1. Fetch both remotes and inspect divergence. Use a clean checkout based on the current fork integration branch so unrelated work remains untouched. Record both parent commit IDs in the PR.
2. Merge the selected upstream commit into a new sync branch. Resolve conflicts by tracing both implementations. Preserve immutable compatible CLI runtime registration, CLI exposure validation, scoped MCP credentials and fork lifecycle behavior while adopting current upstream architecture and installation behavior.
3. Run the repository's current development checks, including relevant translator, controller and installer tests, race checks, vet and `make -C go lint`. Verify that CI runs for PRs and pushes to `feature/mcp-cli`. Obtain an independent review of conflict resolutions and automatically merged fork code. Report missing integration prerequisites and unrun checks in the PR.
4. Commit with DCO sign-off and open a PR targeting `feature/mcp-cli`. Merge after required checks and review pass, with explicit maintainer approval for publication and merge. A completed sync retains both histories and identifies the incorporated upstream commit.

Prefer regular checks of upstream `main` and reviewed sync PRs when it advances. A check cadence or PR automation must be configured explicitly; this guide does not create a schedule. Release tags are useful qualification points, but moving branches and tags are not deployment provenance.

## Estate promotion

An accepted source sync is an input to a separate estate qualification. Build from an immutable fork commit and record that commit, its upstream base and the resulting image digests.

Qualify a matched controller, ADK, role runtime, CRD, chart, UI and Substrate stack. Review database migration and rollback requirements before activation. Register exact compatible CLI runtime image digests; retain previous digests and configuration for rollback.

Coordinate qualification with the estate's current network and policy state, including Cilium rollout. Validate native and CLI tools, credentials, artifacts, human interaction, session continuity, shutdown, Slack flows and denial controls against that state. Use the estate repository's current deployment runbook and explicit deployment approval; preserve live sessions and independent workload lanes.
