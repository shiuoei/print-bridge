package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/transfer"
)

// configTransferFields is the closed set accepted by `--only`. The names match
// the CLI so an ops runbook stays consistent between the GUI and the CLI.
var configTransferFields = transfer.FieldKeys

// runConfigTransfer implements `config export|import`. It always uses the
// live-or-offline read path from the command service, so an active agent is the
// single source of truth and the CLI never corrupts live job state.
func runConfigTransfer(ctx context.Context, dir, action string, args []string) error {
	fs := flag.NewFlagSet("config "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	passwordFile := fs.String("password-file", "", "file containing the transfer password")
	preview := fs.Bool("preview", false, "show the imported config without saving")
	yes := fs.Bool("yes", false, "apply the import")
	allowEmptyPassword := fs.Bool("allow-empty-password", false, "confirm exporting remote-token under an empty password")
	var only multiFlag
	fs.Var(&only, "only", "export only the named field (repeatable)")

	positional, err := splitFlags(args)
	if err != nil {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, err.Error())
	}
	if err := fs.Parse(positional.flagArgs); err != nil {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, err.Error())
	}
	if len(positional.files) < 1 {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, "usage: config "+action+" <file> --password-file <file>")
	}
	file := positional.files[0]

	if action == "import" && *passwordFile == "" {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, "--password-file is required for import")
	}
	password := ""
	if *passwordFile != "" {
		secret, err := os.ReadFile(*passwordFile)
		if err != nil {
			return domain.NewProtocolError(domain.CodePermission, "cannot read password file").WithCause(err)
		}
		// A password file written by an editor normally ends with one newline.
		// Strip only that single terminator so a password that deliberately ends
		// with other whitespace still round-trips unchanged.
		password = strings.TrimSuffix(strings.TrimSuffix(string(secret), "\n"), "\r")
	}

	current, err := loadEffectiveConfig(ctx, dir)
	if err != nil {
		return err
	}

	if action == "export" {
		// --preview/--yes describe an import; accepting them silently on export
		// lets an operator believe the export was validated when it was not.
		rejected := ""
		fs.Visit(func(f *flag.Flag) {
			if (f.Name == "preview" || f.Name == "yes") && rejected == "" {
				rejected = f.Name
			}
		})
		if rejected != "" {
			return domain.NewProtocolErrorf(domain.CodeArgumentInvalid, "--%s is only valid for config import", rejected)
		}
		for _, name := range only {
			if _, ok := configTransferFields[name]; !ok {
				return domain.NewProtocolErrorf(domain.CodeArgumentInvalid, "unknown export field %q; allowed: %s", name, strings.Join(sortedFields(), ", "))
			}
		}
		// An explicit selection exports only those keys; no selection exports the
		// whole transferable subset. Unselected keys are absent from the payload,
		// so importing the file leaves them untouched.
		selection := transfer.SelectTailored(current, only)
		if len(only) == 0 {
			selection = transfer.SelectAll(current)
		}
		// The CLI requires a second confirmation before writing a
		// bearer token under an empty password: the file is still encrypted, but
		// the key is empty, so the token is effectively unprotected on disk.
		if password == "" && onlyIncludesToken(only) && current.Remote.BearerToken != "" && !*allowEmptyPassword {
			return domain.NewProtocolError(domain.CodeArgumentInvalid,
				"exporting remote-token with an empty password leaves the token effectively unprotected; pass --allow-empty-password to confirm")
		}
		data, err := transfer.Export(selection, password)
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return domain.NewProtocolError(domain.CodePermission, "cannot write export file").WithCause(err)
		}
		fmt.Printf("exported %d field(s) to %s\n", len(effectiveFields(only)), file)
		return nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return domain.NewProtocolError(domain.CodePermission, "cannot read import file").WithCause(err)
	}
	imported, err := transfer.Import(data, password, current)
	if err != nil {
		return domain.NewProtocolError(domain.CodeInvalidMessage, "cannot decrypt or parse the import file").WithCause(err)
	}
	diff := diffConfig(current, imported)
	if len(diff) == 0 {
		fmt.Println("no changes; the file matches the current configuration")
		return nil
	}
	if *preview || !*yes {
		fmt.Println("pending changes (sensitive values redacted):")
		for _, line := range diff {
			fmt.Println("  " + line)
		}
		if !*preview {
			fmt.Println("preview only; pass --yes to apply")
		}
		return nil
	}
	if err := saveConfig(ctx, dir, imported); err != nil {
		return err
	}
	fmt.Printf("applied %d change(s)\n", len(diff))
	return nil
}

// splitFlags separates positional arguments from flags so `config export file.json
// --password-file pw` works regardless of order.
func splitFlags(args []string) (struct {
	flagArgs []string
	files    []string
}, error) {
	out := struct {
		flagArgs []string
		files    []string
	}{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			out.files = append(out.files, a)
			continue
		}
		out.flagArgs = append(out.flagArgs, a)
		// A flag that takes a separate value must keep it attached.
		if !strings.Contains(a, "=") && flagTakesValue(strings.TrimLeft(a, "-")) {
			if i+1 >= len(args) {
				return out, fmt.Errorf("flag %s needs a value", a)
			}
			i++
			out.flagArgs = append(out.flagArgs, args[i])
		}
	}
	return out, nil
}

// flagTakesValue reports whether a flag consumes the next argument. The list must
// name every value-taking flag used with splitFlags, or the value would be treated
// as a positional file.
func flagTakesValue(name string) bool {
	switch name {
	case "password-file", "only",
		"format", "days", "status", "query", "limit":
		return true
	}
	return false
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func sortedFields() []string {
	out := make([]string, 0, len(configTransferFields))
	for name := range configTransferFields {
		out = append(out, name)
	}
	// Deterministic order keeps help and error text stable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func effectiveFields(only []string) []string {
	if len(only) == 0 {
		return sortedFields()
	}
	return only
}

// onlyIncludesToken reports whether the selection covers `remote-token`. An empty
// selection covers every field, so it includes the token.
func onlyIncludesToken(only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, name := range only {
		if name == "remote-token" {
			return true
		}
	}
	return false
}

// diffConfig produces a redacted change list. Secrets never print in clear text,
// mirroring the requirement in FEATURES.md S05.
func diffConfig(before, after domain.Config) []string {
	var out []string
	if before.Service.Port != after.Service.Port {
		out = append(out, fmt.Sprintf("service.port: %d -> %d", before.Service.Port, after.Service.Port))
	}
	if strings.Join(before.Security.AllowedOrigins, ",") != strings.Join(after.Security.AllowedOrigins, ",") {
		out = append(out, fmt.Sprintf("security.allowed_origins: %d -> %d entries", len(before.Security.AllowedOrigins), len(after.Security.AllowedOrigins)))
	}
	if strings.Join(before.Security.AllowedIPs, ",") != strings.Join(after.Security.AllowedIPs, ",") {
		out = append(out, fmt.Sprintf("security.allowed_ips: %d -> %d entries", len(before.Security.AllowedIPs), len(after.Security.AllowedIPs)))
	}
	if before.App.Language != after.App.Language {
		out = append(out, fmt.Sprintf("app.language: %s -> %s", before.App.Language, after.App.Language))
	}
	if before.Remote.Enabled != after.Remote.Enabled {
		out = append(out, fmt.Sprintf("remote.enabled: %t -> %t", before.Remote.Enabled, after.Remote.Enabled))
	}
	if before.Remote.EndpointURL != after.Remote.EndpointURL {
		out = append(out, fmt.Sprintf("remote.endpoint_url: %q -> %q", redactURL(before.Remote.EndpointURL), redactURL(after.Remote.EndpointURL)))
	}
	if before.Remote.BearerToken != after.Remote.BearerToken {
		out = append(out, fmt.Sprintf("remote.bearer_token: %s -> %s", redactSecret(before.Remote.BearerToken), redactSecret(after.Remote.BearerToken)))
	}
	if before.Remote.PollIntervalSeconds != after.Remote.PollIntervalSeconds {
		out = append(out, fmt.Sprintf("remote.poll_interval_seconds: %d -> %d", before.Remote.PollIntervalSeconds, after.Remote.PollIntervalSeconds))
	}
	if before.Remote.MaxReportRetries != after.Remote.MaxReportRetries {
		out = append(out, fmt.Sprintf("remote.max_report_retries: %d -> %d", before.Remote.MaxReportRetries, after.Remote.MaxReportRetries))
	}
	return out
}

func redactSecret(v string) string {
	if v == "" {
		return "(empty)"
	}
	return "********"
}

func redactURL(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}

// loadEffectiveConfig prefers the running agent so the CLI never reads a stale
// copy, and only opens the store when the agent is provably absent.
func loadEffectiveConfig(ctx context.Context, dir string) (domain.Config, error) {
	resp, err := ipcCall(ctx, dir, command.Request{ProtocolVersion: 1, RequestID: uuid.NewString(), Command: "config.get"})
	if err == nil && resp.Error == "" && resp.Config != nil {
		return *resp.Config, nil
	}
	return offlineConfig(ctx, dir)
}

// saveConfig prefers the running agent, which keeps its in-memory config and the
// file consistent. Falling back to an exclusive open is only safe when the agent
// is absent.
func saveConfig(ctx context.Context, dir string, c domain.Config) error {
	resp, err := ipcCall(ctx, dir, command.Request{ProtocolVersion: 1, RequestID: uuid.NewString(), Command: "config.set", Config: &c})
	if err == nil {
		if resp.Error != "" {
			return domain.NewProtocolError(domain.CodeOf(nil), resp.Error)
		}
		return nil
	}
	if !agentAbsent(err) {
		return domain.NewProtocolError(domain.CodeInternalError, "agent IPC failed").WithCause(err)
	}
	return offlineSetConfig(ctx, dir, c)
}

var _ = json.Marshal
var _ = errors.Is
