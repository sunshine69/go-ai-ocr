package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const defaultDSN = "file:tfstate.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

var userRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,128}$`) // no ':' (basic auth separator)

func dbFlags(fs *flag.FlagSet) (driver, dsn *string) {
	return fs.String("db-driver", env("TFSTATE_DB_DRIVER", "sqlite"), "sqlite | postgres"),
		fs.String("dsn", env("TFSTATE_DSN", defaultDSN), "database DSN")
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

const userUsage = `usage:
  tfstate-server user add    -project <name|*> -user <name> [-password <pw> | -generate]   (add or reset)
  tfstate-server user delete -user <name>
  tfstate-server user list
common flags: -db-driver, -dsn
password sources for add: -generate, -password, $TFSTATE_NEW_PASSWORD, or first line of stdin`

// userCmd manages credentials directly in the database; the running server
// sees changes immediately (credentials are looked up per request).
func userCmd(args []string) {
	if len(args) < 1 {
		die("%s", userUsage)
	}
	action := args[0]
	fs := flag.NewFlagSet("user "+action, flag.ExitOnError)
	driver, dsn := dbFlags(fs)
	project := fs.String("project", "", `project this user may access, or "*" for all`)
	username := fs.String("user", "", "username")
	password := fs.String("password", os.Getenv("TFSTATE_NEW_PASSWORD"), "password (prefer -generate or stdin; flags leak via ps/history)")
	generate := fs.Bool("generate", false, "generate a random password and print it once")
	fs.Parse(args[1:])

	switch action {
	case "add", "delete", "list":
	default:
		die("%s", userUsage)
	}

	store, err := OpenSQLStore(*driver, *dsn, 0)
	if err != nil {
		die("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	switch action {
	case "list":
		creds, err := store.ListCredentials(ctx)
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("%-32s %-32s %s\n", "USER", "PROJECT", "CREATED")
		for _, c := range creds {
			fmt.Printf("%-32s %-32s %s\n", c.Username, c.Project, c.CreatedAt.Format("2006-01-02 15:04:05"))
		}

	case "delete":
		if *username == "" {
			die("-user is required")
		}
		if err := store.DeleteCredential(ctx, *username); errors.Is(err, ErrNotFound) {
			die("no such user %q", *username)
		} else if err != nil {
			die("%v", err)
		}
		fmt.Printf("deleted %s\n", *username)

	case "add":
		if !userRe.MatchString(*username) {
			die("-user must match %s", userRe)
		}
		if *project != "*" && !nameRe.MatchString(*project) {
			die(`-project must match %s, or be "*"`, nameRe)
		}
		pw := *password
		switch {
		case *generate:
			b := make([]byte, 24)
			if _, err := rand.Read(b); err != nil {
				die("%v", err)
			}
			pw = base64.RawURLEncoding.EncodeToString(b)
		case pw == "":
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			pw = strings.TrimRight(line, "\r\n")
		}
		if len(pw) < 8 || len(pw) > 72 { // bcrypt truncates/rejects beyond 72 bytes
			die("password must be 8-72 bytes")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			die("%v", err)
		}
		if err := store.PutCredential(ctx, Credential{Username: *username, Project: *project, PasswordHash: string(hash)}); err != nil {
			die("%v", err)
		}
		fmt.Printf("user %s -> project %s\n", *username, *project)
		if *generate {
			fmt.Printf("password: %s\n", pw)
		}
	}
}
