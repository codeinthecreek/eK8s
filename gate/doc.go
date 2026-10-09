// Package gate implements the correctness gate's five ordered stages that
// a mutant wrapper must pass before going live (DESIGN.md sec 7).
//
// A run copies the repo's fixed tree and the candidate's files into a
// fresh build tree outside the repo, and runs every go command on it with
// an environment built from scratch. Prerequisite: the module cache holds
// every module (`go mod download` in the repo), since those commands run
// with GOPROXY=off.
package gate
