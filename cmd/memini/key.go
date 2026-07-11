package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/eleboucher/memini/internal/apiauth"
	"github.com/eleboucher/memini/internal/httputil"
	"github.com/eleboucher/memini/internal/store"
)

var (
	keyHome      string
	keyDefaultNS string
	keyDisabled  bool
)

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage local API keys (this store's api_keys table only)",
	Long: "Manage API keys persisted in this store's api_keys table. This is unrelated to " +
		"MEMINI_API_KEYS_FILE, a server-boot concept: keys declared there are loaded once " +
		"when the server starts and cannot be created, rotated, or removed by this command.",
}

var keyAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Create an API key, or rotate an existing one's secret",
	Long: "Create a new named API key, generating a random secret that is printed exactly " +
		"once to stdout — it is never stored and cannot be recovered or shown again, so " +
		"save it now. Re-running with a name that already exists ROTATES that key: a new " +
		"secret is generated and the old one stops authenticating immediately, while the " +
		"key's CreatedAt, home/default namespace bindings, and disabled state are all " +
		"preserved unless the corresponding flag is explicitly passed (an explicit " +
		"--home \"\" clears the binding; an explicit --disabled=false re-enables a " +
		"disabled key — rotation alone never re-enables one).",
	Args: cobra.ExactArgs(1),
	RunE: runKeyAdd,
}

var keyRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Delete an API key",
	Args:  cobra.ExactArgs(1),
	RunE:  runKeyRm,
}

var keyLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List API keys (name, home/default namespace, created, disabled — never secrets or hashes)",
	Args:  cobra.NoArgs,
	RunE:  runKeyLs,
}

func init() {
	keyAddCmd.Flags().StringVar(&keyHome, "home", "",
		"bind the key to a home namespace (default: unbound)")
	keyAddCmd.Flags().StringVar(&keyDefaultNS, "default-namespace", "",
		"namespace applied when a request presents this key with no explicit namespace header")
	keyAddCmd.Flags().BoolVar(&keyDisabled, "disabled", false, "create the key already disabled")

	keyCmd.AddCommand(keyAddCmd, keyRmCmd, keyLsCmd)
	rootCmd.AddCommand(keyCmd)
}

// keyStoreOf type-asserts st to store.APIKeyStore, returning a clear error
// when the configured backend predates api keys (store.APIKeyStore is an
// optional capability interface — mirrors linkStoreOf in link.go).
func keyStoreOf(st store.Store) (store.APIKeyStore, error) {
	ks, ok := st.(store.APIKeyStore)
	if !ok {
		return nil, fmt.Errorf("this storage backend does not support api keys")
	}
	return ks, nil
}

// normalizeOptionalNamespace normalizes and validates ns, treating an empty
// (or all-whitespace/slash) result as "unset" rather than an error — unlike
// normalizeLinkNamespace (link.go), whose src/dst are always required, --home
// and --default-namespace here are optional.
func normalizeOptionalNamespace(ns string) (string, error) {
	ns = httputil.NormalizeNamespace(ns)
	if ns == "" {
		return "", nil
	}
	if err := httputil.ValidateNamespace(ns); err != nil {
		return "", fmt.Errorf("invalid namespace %q: %w", ns, err)
	}
	return ns, nil
}

// generateAPIKeySecret returns a fresh 32-byte hex-encoded (64 char) random
// secret, the plaintext credential handed to the caller exactly once. Only
// its SHA-256 hash (apiauth.HashToken) is ever persisted. Thin wrapper over
// apiauth.GenerateSecret — the one canonical generator, shared with the REST
// key-management API (K3b) so the CLI and REST never risk minting secrets
// two different ways.
func generateAPIKeySecret() (string, error) {
	return apiauth.GenerateSecret()
}

// keyAddOpts carries key add's optional flags with presence tracking: a nil
// field means the flag was not passed this invocation (preserve the existing
// key's value on rotation), while a non-nil pointer — including a pointer to
// the zero value, e.g. an explicit `--home ""` or `--disabled=false` — means
// the operator stated intent and it overrides. runKeyAdd populates it from
// cmd.Flags().Changed.
type keyAddOpts struct {
	Home      *string
	DefaultNS *string
	Disabled  *bool
}

// addAPIKey validates home/defaultNS, generates a fresh secret, and upserts
// the key keyed by name. Re-running with an existing name rotates it: a new
// secret and hash, same row, CreatedAt preserved by PutAPIKey's upsert
// semantics (store.APIKeyStore.PutAPIKey's doc), and every field whose flag
// was not passed this invocation (nil in opts) carried forward from the
// existing row — most critically Disabled: a key disabled during incident
// response must not be silently re-enabled by a later secret rotation.
// Returns the plaintext secret (present exactly here and nowhere else) and
// the stored row as persisted (re-read via GetAPIKeyByHash so CreatedAt
// reflects what was actually written, not just the zero value passed in).
func addAPIKey(ctx context.Context, ks store.APIKeyStore, name string, opts keyAddOpts) (string, store.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", store.APIKey{}, fmt.Errorf("api key name must not be empty")
	}
	// Seed each field from the existing row (if the name already exists) so
	// a rotation only changes what the operator explicitly asked to change.
	// Looked up via ListAPIKeys + filter rather than a new by-name store
	// method: keys are few, and the store contract stays as-is.
	var home, defaultNS string
	var disabled bool
	existing, err := findAPIKeyByName(ctx, ks, name)
	if err != nil {
		return "", store.APIKey{}, fmt.Errorf("add api key %q: look up existing: %w", name, err)
	}
	if existing != nil {
		home, defaultNS, disabled = existing.HomeNS, existing.DefaultNS, existing.Disabled
	}
	if opts.Home != nil {
		h, herr := normalizeOptionalNamespace(*opts.Home)
		if herr != nil {
			return "", store.APIKey{}, herr
		}
		home = h
	}
	if opts.DefaultNS != nil {
		d, derr := normalizeOptionalNamespace(*opts.DefaultNS)
		if derr != nil {
			return "", store.APIKey{}, derr
		}
		defaultNS = d
	}
	if opts.Disabled != nil {
		disabled = *opts.Disabled
	}
	secret, err := generateAPIKeySecret()
	if err != nil {
		return "", store.APIKey{}, err
	}
	key := store.APIKey{
		Name:      name,
		Hash:      apiauth.HashToken(secret),
		HomeNS:    home,
		DefaultNS: defaultNS,
		Disabled:  disabled,
		// CreatedAt intentionally left zero: PutAPIKey stamps "now" for a
		// brand-new row and preserves the existing CreatedAt on rotation.
	}
	if err := ks.PutAPIKey(ctx, key); err != nil {
		return "", store.APIKey{}, fmt.Errorf("add api key %q: %w", name, err)
	}
	stored, err := ks.GetAPIKeyByHash(ctx, key.Hash)
	if err != nil {
		return "", store.APIKey{}, fmt.Errorf("add api key %q: re-read after write: %w", name, err)
	}
	if stored == nil {
		return "", store.APIKey{}, fmt.Errorf("add api key %q: not found immediately after write", name)
	}
	return secret, *stored, nil
}

// findAPIKeyByName returns the stored key with the given name, or nil when
// none exists. Implemented as ListAPIKeys + filter: the store contract has
// no by-name getter (only by-hash, the auth path), keys number in the
// handfuls, and this runs once per CLI invocation — not worth a new store
// method.
func findAPIKeyByName(ctx context.Context, ks store.APIKeyStore, name string) (*store.APIKey, error) {
	keys, err := ks.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if keys[i].Name == name {
			return &keys[i], nil
		}
	}
	return nil, nil
}

// removeAPIKey deletes the named key, erroring when none exists — the same
// not-found convention as link rm (runLinkRm in link.go).
func removeAPIKey(ctx context.Context, ks store.APIKeyStore, name string) error {
	found, err := ks.DeleteAPIKey(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no api key named %q", name)
	}
	return nil
}

func runKeyAdd(cmd *cobra.Command, args []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ks, err := keyStoreOf(st)
		if err != nil {
			return err
		}
		var opts keyAddOpts
		if cmd.Flags().Changed("home") {
			opts.Home = &keyHome
		}
		if cmd.Flags().Changed("default-namespace") {
			opts.DefaultNS = &keyDefaultNS
		}
		if cmd.Flags().Changed("disabled") {
			opts.Disabled = &keyDisabled
		}
		secret, key, err := addAPIKey(cmd.Context(), ks, args[0], opts)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Secret (save this now — it is not stored and cannot be shown again):") //nolint:errcheck
		fmt.Fprintln(out, secret)                                                                 //nolint:errcheck
		fmt.Fprintln(out)                                                                         //nolint:errcheck
		printAPIKeys(out, []store.APIKey{key})
		return nil
	})
}

func runKeyRm(cmd *cobra.Command, args []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ks, err := keyStoreOf(st)
		if err != nil {
			return err
		}
		if err := removeAPIKey(cmd.Context(), ks, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0]) //nolint:errcheck
		return nil
	})
}

func runKeyLs(cmd *cobra.Command, _ []string) error {
	return withLocalStore(cmd.Context(), func(st store.Store) error {
		ks, err := keyStoreOf(st)
		if err != nil {
			return err
		}
		keys, err := ks.ListAPIKeys(cmd.Context())
		if err != nil {
			return err
		}
		printAPIKeys(cmd.OutOrStdout(), keys)
		return nil
	})
}

// printAPIKeys renders keys as a tabwriter table (link.go's printLinks
// style): NAME/HOME/DEFAULT NS/CREATED/DISABLED. Deliberately never prints
// Hash — ListAPIKeys never returns a plaintext secret in the first place
// (store.APIKeyStore's doc), and this is the one place that data reaches
// stdout, so it stays a closed set of columns rather than a generic dump.
func printAPIKeys(out io.Writer, keys []store.APIKey) {
	if len(keys) == 0 {
		fmt.Fprintln(out, "no api keys") //nolint:errcheck
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOME\tDEFAULT NS\tCREATED\tDISABLED") //nolint:errcheck
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\n", //nolint:errcheck
			k.Name, dashIfEmpty(k.HomeNS), dashIfEmpty(k.DefaultNS), k.CreatedAt.Format(time.RFC3339), k.Disabled)
	}
	_ = tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
