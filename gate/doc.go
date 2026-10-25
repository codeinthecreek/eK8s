// Package gate implements the correctness gate's five ordered stages that
// a mutant wrapper must pass before going live (DESIGN.md sec 7).
//
// A run copies the repo's fixed tree and the candidate's files into a
// fresh build tree outside the repo, and runs every go command on it with
// an environment built from scratch. After a preflight that boots a
// fixed gate-built probe the same way (so a host that cannot sandbox is a
// gate fault, not a failed candidate), it boots the static binary
// under bubblewrap, in its own network namespace, reached through a relay
// started with nsenter; builds and runs it as a FROM scratch image; and
// runs Stages 2 and 3 against the bare process. Stage 4 then boots a
// static race build of the same tree the same way, after its own race
// probe preflight, and loads it with concurrent requests.
//
// Prerequisites: the module cache holds every module (`go mod download`
// in the repo), since those go commands run with GOPROXY=off; bwrap and
// nsenter on PATH; unprivileged user namespaces that keep their
// capabilities (on Ubuntu >= 23.10, an AppArmor profile granting bwrap
// userns, preferred, or kernel.apparmor_restrict_unprivileged_userns=0);
// a docker daemon; gcc and a static libc (libc6-dev, glibc-static) for
// Stage 4's race build; kubectl and kind on PATH for Stage 5 (CI pins
// kubectl v1.37.0 and kind v0.33.0, .github/workflows/deploy.yml). A
// missing one is a gate fault (an error from Run), never a candidate
// failure.
package gate
