module github.com/psinetreject/gameap-updater

go 1.27

require github.com/gameap/gameap v0.0.0

// The GameAP module is v4.x but its module path has no /v4 suffix, so it can't
// be `go get`-ed directly. build.sh checks out a matching tag into ./.sdk/gameap.
replace github.com/gameap/gameap => ./.sdk/gameap
