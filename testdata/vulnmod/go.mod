// A module with a known, reachable vulnerability (GO-2021-0113 in
// golang.org/x/text before v0.3.7). `make vulncheck-proof` runs govulncheck
// here and requires it to fail, proving the CI check can fail. It lives
// under testdata/, so the go tool never builds it as part of Gaia.
module example.com/vulnmod

go 1.26.3

require golang.org/x/text v0.3.5
