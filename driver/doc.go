// Package driver is the mutation operator (DESIGN.md sec 12): it runs
// `claude -p` under bubblewrap on a copy of a parent genome's wrapper
// files and returns the edited files, the model's rationale, the call's
// usage and its raw transcript. BuildPrompt builds each call's prompt.
//
// Isolation (DESIGN.md sec 4, sec 12; CLAUDE.md non-negotiables 1 and
// 3): the sandbox's tree is a fresh work directory holding copies of the
// parent's .go files, a fresh home, the claude binary and the system
// libraries and certificates, read-only; never the repo, the module
// cache, /home, /run or /var, so the fixed core's implementation is not
// in it. The environment is built empty and allowlisted. The session
// runs with --tools "Read,Edit" and --restricted, and the driver kills
// it as soon as its first event shows any other tool set, model,
// permission mode, working directory or claude version (checkInit).
// Only the parent's file names are read back, and only as regular
// files.
//
// Prompts are built from lineage records and the fitness parameters
// only: a record carries no gate Detail and no candidate output, so
// neither can reach a prompt (docs/STATUS.md, Parked). The package's
// build never depends on gate, internal/hashcore or the test-vector
// pool (TestDeps).
package driver
