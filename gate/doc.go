// Package gate implements the correctness gate's five ordered stages that
// a mutant wrapper must pass before going live (DESIGN.md sec 7).
//
// A run copies the repo's fixed tree and the candidate's files into a
// fresh build tree outside the repo, and runs every go command on it with
// an environment built from scratch. It then boots the static binary
// under bubblewrap, in its own network namespace, reached through a relay
// started with nsenter; builds and runs it as a FROM scratch image; and
// runs Stages 2 and 3 against the bare process.
//
// Prerequisites: the module cache holds every module (`go mod download`
// in the repo), since those go commands run with GOPROXY=off; bwrap and
// nsenter on PATH; unprivileged user namespaces that keep their
// capabilities (on Ubuntu >= 23.10, an AppArmor profile granting bwrap
// userns, preferred, or kernel.apparmor_restrict_unprivileged_userns=0);
// a docker daemon. A missing one is a gate fault (an error from Run),
// never a candidate failure.
package gate
