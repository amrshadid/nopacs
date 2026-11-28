// Command searchddl prints the statements that make a tenant's search
// tables for one database, "postgres" or "sqlite": the text the tenant
// schema's search migration holds, written from the registry of searchable
// attributes so the two cannot differ. It is a program for making that
// file, not part of any binary.
package main

import (
	"fmt"
	"os"

	"github.com/amrshadid/nopacs/internal/search"
)

func main() {
	var d search.Dialect
	switch {
	case len(os.Args) == 2 && os.Args[1] == "postgres":
		d = search.Postgres
	case len(os.Args) == 2 && os.Args[1] == "sqlite":
		d = search.SQLite
	default:
		fmt.Fprintln(os.Stderr, "usage: searchddl postgres|sqlite")
		os.Exit(2)
	}
	fmt.Print(search.DDL(d))
}
