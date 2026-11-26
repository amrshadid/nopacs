package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// maxCredentialFile bounds a credential read: an S3 secret access key is at
// most 128 bytes, so a larger file is a mistake and is refused, not cut.
const maxCredentialFile = 4096

// storageTypes are the names of the two storage types a person types.
var storageTypes = []string{"dir", "s3"}

// newStorage builds the storage noun.
func (a *app) newStorage() *cobra.Command {
	return group("storage", "Add, list, show, change, remove and test a tenant's storages",
		`A storage is a place a tenant's archived files are written to: a directory on
the server, or a bucket of an S3 object store. Where one may be is the
installation's to decide (the settings storage.directory-roots and
storage.s3-endpoints), so a tenant can never reach another tenant's files.
Every command takes the tenant first. Adding, changing, removing and testing
need the admin role on the tenant; list and show need the reader role.`,
		a.newStorageAdd(), a.newStorageList(), a.newStorageShow(), a.newStorageSet(), a.newStorageRemove(), a.newStorageTest(), a.newStorageStatus())
}

// storageFlags are the flags that describe a storage, for add and for set.
type storageFlags struct {
	typ         string
	root        string
	path        string
	endpoint    string
	region      string
	bucket      string
	prefix      string
	pathStyle   bool
	purge       bool
	accessKeyID string
	credEnv     string
	credFile    string
	capacity    string
	replicaOf   string
	tier        string
	recallDays  int
	recallTier  string
	encrypted   bool
}

// add registers the flags on a command. For add the type is required and the
// others fill it; for set the type follows from the flags given, since a
// storage's type never changes.
func (f *storageFlags) add(cmd *cobra.Command, forSet bool) {
	fl := cmd.Flags()
	if !forSet {
		fl.StringVar(&f.typ, "type", "", "the storage's type: dir (a directory on the server) or s3 (a bucket) (required)")
		_ = cmd.MarkFlagRequired("type")
		_ = cmd.RegisterFlagCompletionFunc("type", cobra.FixedCompletions(storageTypes, cobra.ShellCompDirectiveNoFileComp))
	}
	fl.StringVar(&f.root, "root", "", "dir: the name of the root, as the setting storage.directory-roots names it")
	fl.StringVar(&f.path, "path", "", "dir: the directory below the tenant's directory in the root, segments joined by \"/\"; empty for the tenant's directory itself")
	fl.StringVar(&f.endpoint, "endpoint", "", "s3: the store's base URL, one the setting storage.s3-endpoints lists")
	fl.StringVar(&f.region, "region", "", "s3: the region the bucket is in, such as eu-central-1")
	fl.StringVar(&f.bucket, "bucket", "", "s3: the bucket's name; it must exist, the server never makes one")
	fl.StringVar(&f.prefix, "prefix", "", "s3: the prefix every key is written under, ending in \"/\", such as nopacs/lab-a/; empty for the whole bucket")
	fl.BoolVar(&f.pathStyle, "path-style", false, "s3: address the bucket in the URL's path rather than its host name; most stores other than AWS need it, and the server requires it at an endpoint the installation allows private addresses at")
	fl.BoolVar(&f.purge, "purge-versions", true, "s3: on a bucket that keeps versions, a purge of a correction deletes every older version of each object it removes, so the bytes are gone; --purge-versions=false leaves them, hidden behind a delete marker")
	fl.StringVar(&f.accessKeyID, "access-key-id", "", "s3: the access key id of the credentials the server uses (it is not a secret)")
	fl.StringVar(&f.credEnv, "credential-env", "", "s3: name of the environment variable that holds the secret access key that goes with --access-key-id")
	fl.StringVar(&f.credFile, "credential-file", "", "s3: path of a file, readable only by you, that holds the secret access key that goes with --access-key-id")
	fl.StringVar(&f.capacity, "capacity", "", "the most bytes the storage may hold, a size such as 500GiB; 0 for no limit of its own")
	cmd.MarkFlagsMutuallyExclusive("credential-env", "credential-file")
	if !forSet {
		fl.StringVar(&f.replicaOf, "replica-of", "", "make the storage a replica of this primary of the tenant from the start; the server then copies what the primary holds")
	} else {
		fl.StringVar(&f.replicaOf, "replica-of", "", "refused here: a storage becomes a replica when it is added with --replica-of, or by \"replica attach\"")
	}
	fl.StringVar(&f.tier, "tier", "", "online (every object reads at once, the default) or archive (some objects must be recalled first; a replica on S3 only)")
	_ = cmd.RegisterFlagCompletionFunc("tier", cobra.FixedCompletions(storageTierNames, cobra.ShellCompDirectiveNoFileComp))
	fl.IntVar(&f.recallDays, "recall-days", 0, "s3, archive tier: how many days a recalled object stays readable (1 to 365)")
	fl.StringVar(&f.recallTier, "recall-tier", "", "s3, archive tier: how fast the store brings an object back: standard, bulk or expedited")
	_ = cmd.RegisterFlagCompletionFunc("recall-tier", cobra.FixedCompletions(recallTierNames, cobra.ShellCompDirectiveNoFileComp))
	fl.BoolVar(&f.encrypted, "encrypted-at-rest-declared", false, "dir: state that the volume under the directory encrypts what it holds at rest; the status shows it as declared, never as checked")
}

// storageTierNames and recallTierNames are what a person may type for the
// tier flags.
var (
	storageTierNames = []string{"online", "archive"}
	recallTierNames  = []string{"standard", "bulk", "expedited"}
)

// parseStorageTier reads --tier.
func parseStorageTier(s string) (v1.StorageTier, error) {
	switch strings.ToLower(s) {
	case "online":
		return v1.StorageTier_STORAGE_TIER_ONLINE, nil
	case "archive":
		return v1.StorageTier_STORAGE_TIER_ARCHIVE, nil
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--tier %q is not a tier; use online or archive", output.Clean(s)))
}

// parseRecallTier reads --recall-tier.
func parseRecallTier(s string) (v1.S3RecallTier, error) {
	switch strings.ToLower(s) {
	case "standard":
		return v1.S3RecallTier_S3_RECALL_TIER_STANDARD, nil
	case "bulk":
		return v1.S3RecallTier_S3_RECALL_TIER_BULK, nil
	case "expedited":
		return v1.S3RecallTier_S3_RECALL_TIER_EXPEDITED, nil
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--recall-tier %q is not a recall tier; use standard, bulk or expedited", output.Clean(s)))
}

// readCredential reads an S3 secret access key from the environment variable
// named by env or from the private file named by file, and from nowhere else.
// The messages name the variable or the file and never show any of the
// secret. It returns "" when neither is named.
func (a *app) readCredential(env, file string) (string, error) {
	var v string
	switch {
	case env != "":
		v = strings.TrimSpace(a.env.Getenv(env))
		if v == "" {
			return "", fault.New(fault.Invalid, "--credential-env names "+env+", which is not set or is empty")
		}
	case file != "":
		// The same rule as the key file: a secret others can read is a secret
		// others can use.
		f, err := secret.OpenPrivate(file)
		if err != nil {
			msg := credentialFileRefusal(err)
			if fault.KindOf(err) == fault.NotFound {
				msg = "cannot read --credential-file " + file + ": it does not exist"
			}
			return "", fault.Wrap(fault.Invalid, err, msg)
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxCredentialFile+1))
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read --credential-file "+file)
		}
		if len(b) > maxCredentialFile {
			return "", fault.New(fault.Invalid, file+" holds more than a secret access key can")
		}
		v = strings.TrimSpace(string(b))
		if v == "" {
			return "", fault.New(fault.Invalid, "--credential-file "+file+" is empty")
		}
	default:
		return "", nil
	}
	if len(v) > maxCredentialFile {
		return "", fault.New(fault.Invalid, "--credential-env "+env+" holds more than a secret access key can")
	}
	return v, nil
}

// given says whether any of the named flags was set on the command line.
func (f *storageFlags) given(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// dirNames and s3Names are the flags of each type; --capacity belongs to both.
var (
	dirNames = []string{"root", "path"}
	s3Names  = []string{"endpoint", "region", "bucket", "prefix", "path-style", "purge-versions", "access-key-id", "credential-env", "credential-file"}
)

// applyReplication puts the replication and encryption flags of an added
// storage in it. The flags of the other type are refused here, since the
// server would refuse them with a less direct message.
func (f *storageFlags) applyReplication(cmd *cobra.Command, st *v1.Storage) error {
	st.ReplicaOf = f.replicaOf
	if cmd.Flags().Changed("tier") {
		t, err := parseStorageTier(f.tier)
		if err != nil {
			return err
		}
		st.Tier = t
	}
	if f.given(cmd, "recall-days", "recall-tier") {
		s3 := st.GetS3()
		if s3 == nil {
			return fault.New(fault.Invalid, "--recall-days and --recall-tier belong to an S3 storage of the archive tier")
		}
		if cmd.Flags().Changed("recall-days") {
			d, err := boundedInt32("recall-days", f.recallDays, 1, 365)
			if err != nil {
				return err
			}
			s3.RecallDays = d
		}
		if cmd.Flags().Changed("recall-tier") {
			t, err := parseRecallTier(f.recallTier)
			if err != nil {
				return err
			}
			s3.RecallTier = t
		}
	}
	if cmd.Flags().Changed("encrypted-at-rest-declared") {
		d := st.GetDirectory()
		if d == nil {
			return fault.New(fault.Invalid, "--encrypted-at-rest-declared belongs to a directory storage: an S3 storage's encryption is read from the bucket")
		}
		d.EncryptedAtRestDeclared = f.encrypted
	}
	return nil
}

// newStorageAdd builds "storage add".
func (a *app) newStorageAdd() *cobra.Command {
	var f storageFlags
	cmd := &cobra.Command{
		Use:   "add <tenant> <name> --type dir --root ROOT [--path PATH] | --type s3 --endpoint URL --region R --bucket B --access-key-id ID (--credential-env VAR | --credential-file PATH)",
		Short: "Add a storage to a tenant",
		Long: `Add a storage to a tenant. A directory storage is a directory below a root the
installation names; the server makes it, and it must be absent or empty. An S3
storage is a bucket (or a prefix in one) at an endpoint the installation lists;
the bucket must exist.

The secret access key of an S3 storage is a secret, so it is never an argument:
it is read from the environment variable named by --credential-env or from the
file named by --credential-file (a file only you can open), sent to the server
over the connection, sealed there, and never printed or answered again. The
answer only says whether one is held.

A storage's name is 1 to 63 lowercase letters, digits and hyphens, starting with
a letter. The commands "storage test" and "tenant status" tell whether it can be
written to and how much room it has.`,
		Example: `  nopacs storage add lab1 main --type dir --root fast --path main
  export LAB1_S3_SECRET="$(cat /etc/nopacs/lab1.s3)"
  nopacs storage add lab1 cold --type s3 --endpoint https://s3.eu-central-1.amazonaws.com \
      --region eu-central-1 --bucket lab1-archive --prefix nopacs/ \
      --access-key-id AKIAEXAMPLE --credential-env LAB1_S3_SECRET --capacity 2TiB`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			st := &v1.Storage{Name: args[1]}
			switch strings.ToLower(f.typ) {
			case "dir", "directory":
				if f.given(cmd, s3Names...) {
					return fault.New(fault.Invalid, "--type dir takes --root and --path, not the flags of an S3 storage")
				}
				st.Type = &v1.Storage_Directory{Directory: &v1.DirectoryStorage{Root: f.root, Path: f.path}}
			case "s3":
				if f.given(cmd, dirNames...) {
					return fault.New(fault.Invalid, "--type s3 takes --endpoint, --region, --bucket and the credentials, not --root or --path")
				}
				if f.accessKeyID == "" || (f.credEnv == "" && f.credFile == "") {
					return fault.New(fault.Invalid, "an S3 storage needs --access-key-id and its secret access key from --credential-env or --credential-file")
				}
				cred, err := a.readCredential(f.credEnv, f.credFile)
				if err != nil {
					return err
				}
				st.Type = &v1.Storage_S3{S3: &v1.S3Storage{
					Endpoint: f.endpoint, Region: f.region, Bucket: f.bucket, Prefix: f.prefix, PathStyle: f.pathStyle,
					AccessKeyId: f.accessKeyID, SecretAccessKey: cred,
				}}
				if cmd.Flags().Changed("purge-versions") {
					st.GetS3().PurgeVersions = &f.purge
				}
			default:
				return fault.New(fault.Invalid, fmt.Sprintf("--type %q is not a storage type; use dir or s3", output.Clean(f.typ)))
			}
			if cmd.Flags().Changed("capacity") {
				n, err := parseSize("capacity", f.capacity)
				if err != nil {
					return err
				}
				st.CapacityBytes = n
			}
			if err := f.applyReplication(cmd, st); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The request holds the secret; only the response, which never
			// does, is printed.
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AddStorageResponse, error) {
				return c.Storages.AddStorage(ctx, &v1.AddStorageRequest{Tenant: args[0], Storage: st})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.storageDetails(w, res.GetStorage()) })
		}),
	}
	f.add(cmd, false)
	return cmd
}

// capacityText words a storage's own size limit.
func capacityText(n int64) string {
	if n == 0 {
		return "no limit"
	}
	return output.Bytes(n)
}

// olderVersionsText words what a purge does with the older versions of an
// object on a bucket that keeps versions; purge_versions absent means true.
func olderVersionsText(s *v1.S3Storage) string {
	if s.PurgeVersions != nil && !s.GetPurgeVersions() {
		return "kept by a purge (--purge-versions=false)"
	}
	return "deleted by a purge"
}

// storageDetails writes one storage as labeled lines. A secret is never one
// of them: the server answers only whether one is held.
func (a *app) storageDetails(w io.Writer, st *v1.Storage) error {
	pairs := []output.Pair{
		{Label: "Name", Value: st.GetName()},
		{Label: "Type", Value: output.StorageType(st)},
	}
	switch t := st.GetType().(type) {
	case *v1.Storage_Directory:
		pairs = append(pairs,
			output.Pair{Label: "Root", Value: t.Directory.GetRoot()},
			output.Pair{Label: "Path", Value: output.Dash(t.Directory.GetPath())},
			output.Pair{Label: "Encrypted at rest", Value: declaredText(t.Directory.GetEncryptedAtRestDeclared())})
	case *v1.Storage_S3:
		held := "not set"
		if t.S3.GetSecretAccessKeySet() {
			held = "set"
		}
		style := "host name"
		if t.S3.GetPathStyle() {
			style = "path"
		}
		pairs = append(pairs,
			output.Pair{Label: "Endpoint", Value: t.S3.GetEndpoint()},
			output.Pair{Label: "Region", Value: t.S3.GetRegion()},
			output.Pair{Label: "Bucket", Value: t.S3.GetBucket()},
			output.Pair{Label: "Prefix", Value: output.Dash(t.S3.GetPrefix())},
			output.Pair{Label: "Addressing", Value: style},
			output.Pair{Label: "Older versions", Value: olderVersionsText(t.S3)},
			output.Pair{Label: "Access key id", Value: t.S3.GetAccessKeyId()},
			output.Pair{Label: "Secret access key", Value: held})
		if st.GetTier() == v1.StorageTier_STORAGE_TIER_ARCHIVE {
			pairs = append(pairs,
				output.Pair{Label: "Recall days", Value: strconv.Itoa(int(t.S3.GetRecallDays()))},
				output.Pair{Label: "Recall tier", Value: output.RecallTier(t.S3.GetRecallTier())})
		}
	}
	pairs = append(pairs,
		output.Pair{Label: "Tier", Value: output.StorageTier(st.GetTier())},
		output.Pair{Label: "Replica of", Value: output.Dash(output.Clean(st.GetReplicaOf()))},
		output.Pair{Label: "Switch generation", Value: strconv.FormatInt(st.GetSwitchGeneration(), 10)},
		output.Pair{Label: "Capacity", Value: capacityText(st.GetCapacityBytes())},
		output.Pair{Label: "Created", Value: a.when(st.GetCreateTime())},
		output.Pair{Label: "Updated", Value: a.when(st.GetUpdateTime())})
	return output.Details(w, a.style(), pairs)
}

// declaredText words a directory's own statement about encryption at rest.
func declaredText(declared bool) string {
	if declared {
		return "declared by the admin (not checked by the server)"
	}
	return "not declared"
}

// newStorageList builds "storage list".
func (a *app) newStorageList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant>",
		Short: "List a tenant's storages",
		Long: `List a tenant's storages ordered by name, with their type, where their files go
and the most they may hold. No secret is here: the server never answers one.
Used and free space are part of the tenant's status.`,
		Example: `  nopacs storage list lab1
  nopacs storage list lab1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			storages, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Storage, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListStoragesResponse, error) {
					return c.Storages.ListStorages(ctx, &v1.ListStoragesRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetStorages(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "storages", storages, more, func() error {
				rows := make([][]string, 0, len(storages))
				for _, st := range storages {
					rows = append(rows, []string{st.GetName(), output.StorageType(st), output.StorageTier(st.GetTier()), output.Dash(output.Clean(st.GetReplicaOf())), output.StorageWhere(st), capacityText(st.GetCapacityBytes()), a.when(st.GetCreateTime())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"NAME", "TYPE", "TIER", "REPLICA OF", "LOCATION", "CAPACITY", "CREATED"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}

// newStorageShow builds "storage show".
func (a *app) newStorageShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <name>",
		Short: "Show one storage",
		Long: `Show one storage of a tenant: its type, where its files go, its limit and when
it was added and last changed. For an S3 storage it says whether a secret access
key is held, never the key.`,
		Example: `  nopacs storage show lab1 main`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStorageResponse, error) {
				return c.Storages.GetStorage(ctx, &v1.GetStorageRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.storageDetails(w, res.GetStorage()) })
		}),
	}
}

// newStorageSet builds "storage set".
func (a *app) newStorageSet() *cobra.Command {
	var f storageFlags
	cmd := &cobra.Command{
		Use:   "set <tenant> <name> [--capacity SIZE] [directory or S3 flags]",
		Short: "Change some fields of a storage",
		Long: `Change the fields of a storage that the flags name, and no other. A flag that
is given changes its field even when its value is empty (--prefix "" returns to
the whole bucket); a field without a flag keeps its value, a secret access key
included, so a change of the limit does not need the secret again.

A storage's type never changes. Once it holds an archived file, the fields that
say where its files are (--root, --path, --endpoint, --bucket, --prefix) cannot
change. --access-key-id and its secret change together: give both, the secret
from --credential-env or --credential-file. So does a change of --endpoint:
credentials given for one store are never sent to another, so it is given with
--access-key-id and its secret, and the server refuses it otherwise.
--purge-versions changes only what later purges do with older versions on a
bucket that keeps them, so it may change at any time.`,
		Example: `  nopacs storage set lab1 main --capacity 1TiB
  nopacs storage set lab1 cold --access-key-id AKIANEW --credential-file /etc/nopacs/new.s3
  nopacs storage set lab1 cold --recall-days 14 --recall-tier bulk
  nopacs storage set lab1 main --encrypted-at-rest-declared`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			st := &v1.Storage{Name: args[1]}
			var paths []string
			dir, s3 := &v1.DirectoryStorage{}, &v1.S3Storage{}
			if f.given(cmd, dirNames...) && f.given(cmd, s3Names...) {
				return fault.New(fault.Invalid, "a storage has one type: give the flags of a directory (--root, --path) or of S3, not both")
			}
			if cmd.Flags().Changed("capacity") {
				n, err := parseSize("capacity", f.capacity)
				if err != nil {
					return err
				}
				st.CapacityBytes = n
				paths = append(paths, "capacity_bytes")
			}
			for _, p := range []struct {
				flag, path string
				set        func()
			}{
				{"root", "directory.root", func() { dir.Root = f.root }},
				{"path", "directory.path", func() { dir.Path = f.path }},
				{"endpoint", "s3.endpoint", func() { s3.Endpoint = f.endpoint }},
				{"region", "s3.region", func() { s3.Region = f.region }},
				{"bucket", "s3.bucket", func() { s3.Bucket = f.bucket }},
				{"prefix", "s3.prefix", func() { s3.Prefix = f.prefix }},
				{"path-style", "s3.path_style", func() { s3.PathStyle = f.pathStyle }},
				{"purge-versions", "s3.purge_versions", func() { s3.PurgeVersions = &f.purge }},
			} {
				if cmd.Flags().Changed(p.flag) {
					p.set()
					paths = append(paths, p.path)
				}
			}
			haveID, haveCred := cmd.Flags().Changed("access-key-id"), f.credEnv != "" || f.credFile != ""
			if haveID != haveCred {
				return fault.New(fault.Invalid, "--access-key-id and its secret access key (--credential-env or --credential-file) change together: give both")
			}
			if haveID {
				cred, err := a.readCredential(f.credEnv, f.credFile)
				if err != nil {
					return err
				}
				s3.AccessKeyId, s3.SecretAccessKey = f.accessKeyID, cred
				paths = append(paths, "s3.access_key_id", "s3.secret_access_key")
			}
			if cmd.Flags().Changed("replica-of") {
				return fault.New(fault.Invalid, "--replica-of cannot be changed here: add the storage with --replica-of, or make an existing empty storage a replica with \"nopacs replica attach\"")
			}
			if cmd.Flags().Changed("tier") {
				t, err := parseStorageTier(f.tier)
				if err != nil {
					return err
				}
				st.Tier = t
				paths = append(paths, "tier")
			}
			if cmd.Flags().Changed("recall-days") {
				d, err := boundedInt32("recall-days", f.recallDays, 1, 365)
				if err != nil {
					return err
				}
				s3.RecallDays = d
				paths = append(paths, "s3.recall_days")
			}
			if cmd.Flags().Changed("recall-tier") {
				t, err := parseRecallTier(f.recallTier)
				if err != nil {
					return err
				}
				s3.RecallTier = t
				paths = append(paths, "s3.recall_tier")
			}
			if cmd.Flags().Changed("encrypted-at-rest-declared") {
				dir.EncryptedAtRestDeclared = f.encrypted
				paths = append(paths, "directory.encrypted_at_rest_declared")
			}
			if len(paths) == 0 {
				return fault.New(fault.Invalid, "nothing to change: give at least one flag, such as --capacity")
			}
			switch {
			case f.given(cmd, dirNames...), f.given(cmd, "encrypted-at-rest-declared"):
				st.Type = &v1.Storage_Directory{Directory: dir}
			case f.given(cmd, s3Names...), f.given(cmd, "recall-days", "recall-tier"):
				st.Type = &v1.Storage_S3{S3: s3}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetStorageResponse, error) {
				return c.Storages.SetStorage(ctx, &v1.SetStorageRequest{Tenant: args[0], Storage: st, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.storageDetails(w, res.GetStorage()) })
		}),
	}
	f.add(cmd, true)
	return cmd
}

// newStorageRemove builds "storage remove".
func (a *app) newStorageRemove() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <tenant> <name> --yes",
		Short: "Remove a storage's configuration (no file is deleted)",
		Long: `Remove a storage's configuration from a tenant. It never deletes a file: it is
refused while the storage holds any archived file or while a receiver names it
as its storage, so the configuration of a storage that holds data is never lost.
A repeated command finds no storage and exits 4.

Without --yes nothing is sent: the command says what it would do and exits 2.`,
		Example: `  nopacs storage remove lab1 old          # says what would happen, exits 2
  nopacs storage remove lab1 old --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if !yes {
				return fault.New(fault.Invalid, fmt.Sprintf("nothing was removed: this would remove the configuration of storage %q of tenant %q; no file is deleted; run it again with --yes to do it", output.Clean(args[1]), output.Clean(args[0])))
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemoveStorageResponse, error) {
				return c.Storages.RemoveStorage(ctx, &v1.RemoveStorageRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "storage %s of tenant %s removed (%s); no file was deleted\n", output.Clean(args[1]), output.Clean(args[0]), output.StorageType(res.GetStorage()))
				return err
			})
		}),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "really remove the configuration; without it the command only says what it would do")
	return cmd
}

// newStorageTest builds "storage test".
func (a *app) newStorageTest() *cobra.Command {
	return &cobra.Command{
		Use:   "test <tenant> <name>",
		Short: "Write, read back, compare and remove one test object",
		Long: `Test that a storage can be written to: the server writes one small object of
random bytes under a prefix no archived file uses, reads it back, compares its
SHA-256 with what was written, removes it, and reports each step. The test never
touches data, and repeating it leaves the storage as it was.

A storage that fails a step is a result, not an error: the table says which step
failed and why, and the command exits 1 so a script sees it. A step after a failed
one is skipped, except the removal.`,
		Example: `  nopacs storage test lab1 main`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.TestStorageResponse, error) {
				return c.Storages.TestStorage(ctx, &v1.TestStorageRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			if err := messageOut(a, res, func(w io.Writer) error {
				rows := make([][]string, 0, len(res.GetResults()))
				for _, r := range res.GetResults() {
					took := "-"
					if r.GetDuration() != nil {
						took = r.GetDuration().AsDuration().Round(time.Millisecond).String()
					}
					rows = append(rows, []string{output.StorageTestStep(r.GetStep()), output.StorageTestOutcome(r.GetOutcome()), took, output.StorageFault(r.GetFault())})
				}
				return output.Table(w, a.style(), []string{"STEP", "OUTCOME", "TOOK", "FAULT"}, rows)
			}); err != nil {
				return err
			}
			if !res.GetPassed() {
				return fault.New(fault.Internal, fmt.Sprintf("storage %s of tenant %s failed its test", output.Clean(args[1]), output.Clean(args[0])))
			}
			return nil
		}),
	}
}
