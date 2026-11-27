// Command vulnmod calls language.Parse from a golang.org/x/text version with
// a known vulnerability (GO-2021-0113), for `make vulncheck-proof`.
package main

import (
	"fmt"
	"os"

	"golang.org/x/text/language"
)

func main() {
	tag, err := language.Parse(os.Args[1])
	fmt.Println(tag, err)
}
