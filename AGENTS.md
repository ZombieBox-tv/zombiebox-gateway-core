# Component agent instructions

- English source/docs/commits. Follow the central workspace specification and ADRs when present.
- This is an independent repository. Commit only this component; never commit `.deps`, sibling repositories, SDKs, secrets or build outputs.
- `dependencies.lock.json` pins shared repositories. `make deps-check` verifies reproducible pins; path resolution permits explicit local development. Never duplicate shared business logic or transport code.
- Run `make format` and `make format-check`, then the checks listed in README. Use CodeGraph first when `.codegraph` exists; do not install Graphify or edit upstream clones.
- `monorepo/*` tags preserve filtered historical checkpoints; they are not independent release or milestone evidence. Component completion requires its own checks plus the global product gates.
