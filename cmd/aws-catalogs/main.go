// Command aws-catalogs writes the provider's API catalogs (mamela-catalog/v1) into a directory:
// build.sh writes them into the Pome (pome/catalogs).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ieyoukan/mamela-aws/catalogs"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: aws-catalogs <dir>")
		os.Exit(2)
	}
	for name, c := range catalogs.All {
		data, err := catalogs.Encode(c)
		if err == nil {
			err = os.WriteFile(filepath.Join(os.Args[1], name+".json"), data, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
