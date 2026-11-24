// Package place holds the rules of where a tenant's data may be, and opens
// it there: a directory storage under a root the installation lists
// (storage.directory-roots), inside that root's directory for the tenant and
// reached through no symbolic link below the root; an S3 storage at an
// endpoint the installation lists (storage.s3-endpoints), with the address
// check and the addressing that endpoint's entry allows; a receiver on a
// port the installation allotted to the tenant (receiver.ports) and an
// address it lists (receiver.bind-addresses). The API's handlers check a
// storage or a receiver here before it is stored, and the server opens a
// tenant's storage here for a test or for the reception pipeline, and
// applies the rules again here (Opener.Check) at every use of a store the
// pipeline keeps open, so a rule is never applied one way when a storage is
// added and another way when it is written to.
//
// Each check that touches the file system is made again every time a
// directory is opened or checked, because what lies on the disk can change after a
// storage is added: a root's path can come to pass through a new symbolic
// link, and a link can be planted below a root. Two roots that lead to one
// directory once their links are followed, or one inside the other, are
// refused when the setting is written and again whenever either is used.
//
// The package reads what it needs through small interfaces (Values,
// ReceiverValues, Secrets, Keys), so it never knows which database holds a
// setting or a sealed secret, and a test hands it fakes.
//
// # What this package must never do
//
// It must never import the API's handlers (internal/api) or the reception
// pipeline (internal/receive): both depend on it, and the rules here must be
// one for both (.golangci.yml, depguard "place"). It must never write a
// root's path, a directory's path or a secret into an error, a log or an
// answer: a root's path is the installation's, and an error reaches tenants;
// an error keeps the file system's cause for errors.Is with the path taken
// out (pathless). It must never make or remove anything on a storage but the
// directories of a storage being added (Directory.Make), and never follow a
// symbolic link below a root.
package place
