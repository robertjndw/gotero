package accounts

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
)

// CLIUsage documents the account subcommands for gotero's help text.
const CLIUsage = `  gotero user add <username> (--password-stdin | --password P) [--email E] [--admin] [--id N]
  gotero user list
  gotero user passwd <username> --password-stdin
  gotero user disable <username>
  gotero user enable <username>
  gotero user delete <username> --yes
  gotero invite create [--admin] [--email E] [--hours 72]`

// RunCLI executes "user ..." and "invite ..." commands. It lives here, next
// to the rules it enforces, so that the CLI and the web UI cannot drift
// apart. args starts with the command name.
func (s *Service) RunCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage:\n%s", CLIUsage)
	}
	c := cli{s: s, in: stdin, out: stdout, errw: stderr}
	switch args[0] + " " + args[1] {
	case "user add":
		return c.userAdd(ctx, args[2:])
	case "user list":
		return c.userList(ctx, args[2:])
	case "user passwd":
		return c.userPasswd(ctx, args[2:])
	case "user disable":
		return c.userSetDisabled(ctx, args[2:], true)
	case "user enable":
		return c.userSetDisabled(ctx, args[2:], false)
	case "user delete":
		return c.userDelete(ctx, args[2:])
	case "invite create":
		return c.inviteCreate(ctx, args[2:])
	}
	return fmt.Errorf("unknown command %q\nusage:\n%s", args[0]+" "+args[1], CLIUsage)
}

type cli struct {
	s    *Service
	in   io.Reader
	out  io.Writer
	errw io.Writer
}

// parse lets flags appear before or after positional arguments, which the
// stdlib flag package does not do on its own ("user add bob --admin").
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (c cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.errw)
	return fs
}

func (c cli) oneUsername(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", fmt.Errorf("%s: expected exactly one username", fs.Name())
	}
	return pos[0], nil
}

func (c cli) password(stdin bool, literal string) (string, error) {
	switch {
	case stdin && literal != "":
		return "", errors.New("use either --password-stdin or --password, not both")
	case stdin:
		line, err := bufio.NewReader(c.in).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	case literal != "":
		return literal, nil
	}
	return "", errors.New("a password is required: pass --password-stdin (preferred) or --password")
}

func (c cli) find(ctx context.Context, username string) (repo.User, error) {
	u, err := c.s.store.Users().GetByUsername(ctx, username)
	if errors.Is(err, repo.ErrNotFound) {
		return u, fmt.Errorf("user %q not found", username)
	}
	return u, err
}

// describe turns validation and conflict errors into one readable line.
func describe(err error) error {
	var ve *ValidationError
	var ce *ConflictError
	switch {
	case errors.As(err, &ve):
		return fmt.Errorf("invalid input: %s", ve.Error()[len("validation failed: "):])
	case errors.As(err, &ce):
		parts := make([]string, 0, len(ce.Fields))
		for k, v := range ce.Fields {
			parts = append(parts, k+": "+v)
		}
		return fmt.Errorf("conflict: %s", strings.Join(parts, "; "))
	}
	return err
}

func (c cli) userAdd(ctx context.Context, args []string) error {
	fs := c.flagSet("user add")
	pwStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	pw := fs.String("password", "", "password (visible in the process list; prefer --password-stdin)")
	email := fs.String("email", "", "email address")
	admin := fs.Bool("admin", false, "make the user an administrator")
	id := fs.Int64("id", 0, "explicit user ID, e.g. the old zotero.org user ID")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("user add: expected exactly one username")
	}
	if *id < 0 {
		return errors.New("--id must be positive")
	}
	password, err := c.password(*pwStdin, *pw)
	if err != nil {
		return err
	}
	u, err := c.s.CreateUser(ctx, CreateUserInput{ID: *id, Username: pos[0], Password: password, Email: *email, IsAdmin: *admin, EmailVerified: true})
	if err != nil {
		return describe(err)
	}
	_, err = fmt.Fprintf(c.out, "created user %s (id %d)\n", u.Username, u.ID)
	return err
}

func (c cli) userList(ctx context.Context, args []string) error {
	fs := c.flagSet("user list")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	users, err := c.s.store.Users().List(ctx, "", 0, 0)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tUSERNAME\tROLE\tSTATUS\tEMAIL")
	for _, u := range users {
		role, status := "user", "active"
		if u.IsAdmin {
			role = "admin"
		}
		if u.Disabled {
			status = "disabled"
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", u.ID, u.Username, role, status, u.Email)
	}
	return w.Flush()
}

func (c cli) userPasswd(ctx context.Context, args []string) error {
	fs := c.flagSet("user passwd")
	pwStdin := fs.Bool("password-stdin", false, "read the new password from stdin")
	name, err := c.oneUsername(fs, args)
	if err != nil {
		return err
	}
	if !*pwStdin {
		return errors.New("user passwd: pass --password-stdin and provide the password on stdin")
	}
	password, err := c.password(true, "")
	if err != nil {
		return err
	}
	u, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if err := c.s.SetPassword(ctx, u.ID, password); err != nil {
		return describe(err)
	}
	_, _ = fmt.Fprintf(c.out, "password changed for %s; web sessions were ended\n", u.Username)
	return nil
}

func (c cli) userSetDisabled(ctx context.Context, args []string, disabled bool) error {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	name, err := c.oneUsername(c.flagSet("user "+verb), args)
	if err != nil {
		return err
	}
	u, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if _, err := c.s.UpdateUser(ctx, u.ID, UserPatch{Disabled: &disabled}, 0); err != nil {
		return describe(err)
	}
	_, _ = fmt.Fprintf(c.out, "%sd user %s\n", verb, u.Username)
	if disabled {
		// The CLI has no handle on the running server's key cache.
		_, _ = fmt.Fprintln(c.errw, "note: a running server may accept the user's API keys for up to 30 seconds")
	}
	return nil
}

func (c cli) userDelete(ctx context.Context, args []string) error {
	fs := c.flagSet("user delete")
	yes := fs.Bool("yes", false, "confirm that the user and all their data will be deleted")
	name, err := c.oneUsername(fs, args)
	if err != nil {
		return err
	}
	if !*yes {
		return errors.New("user delete removes the account and its entire library; pass --yes to confirm")
	}
	u, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if err := c.s.DeleteUser(ctx, u.ID, 0); err != nil {
		return describe(err)
	}
	_, _ = fmt.Fprintf(c.out, "deleted user %s\n", u.Username)
	return nil
}

func (c cli) inviteCreate(ctx context.Context, args []string) error {
	fs := c.flagSet("invite create")
	admin := fs.Bool("admin", false, "the invited user becomes an administrator")
	email := fs.String("email", "", "lock the invite to this email address")
	hours := fs.Int("hours", 72, "validity in hours")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("invite create takes no arguments")
	}
	_, token, err := c.s.CreateInvite(ctx, CreateInviteInput{
		Email: *email, IsAdmin: *admin, TTL: time.Duration(*hours) * time.Hour,
	})
	if err != nil {
		return describe(err)
	}
	base := c.s.cfg.PublicURL
	if base == "" {
		_, _ = fmt.Fprintln(c.errw, "warning: GOTERO_PUBLIC_URL is not set, prepend your server's address to this link")
	}
	_, err = fmt.Fprintf(c.out, "%s/register?invite=%s\n", base, token)
	return err
}
