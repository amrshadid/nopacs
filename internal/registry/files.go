package registry

// filesPerTenant is how many files one tenant's store is taken to hold open
// when its driver does not say: the figure of a SQLite tenant (its database
// file and write-ahead log for each of five connections, and the
// shared-memory file; driver.SQLiteFilesPerTenant has the account).
const filesPerTenant = 11

// FileCounter is an optional interface of a Driver: how many files or
// sockets one tenant's store can hold open at most. The registry uses it for
// its warning about the limit on open files, so that a Postgres tenant,
// which holds as many sockets as its pool has connections, is not counted as
// a SQLite tenant with its eleven files. A Driver that does not have it is
// counted as filesPerTenant.
type FileCounter interface {
	FilesPerTenant() int
}

// fileMargin is the files the process needs besides the tenants: the master
// database (as many as a tenant), the key file, the listening sockets and
// the local socket, the API's client connections, and the logs. 256 leaves
// room for a few hundred concurrent clients on top of them.
const fileMargin = 256

// warnFileLimit logs one warning, with both numbers, when the tenants would
// need more open files than the process may have. It does not refuse to
// start: beyond the limit a tenant whose open fails is unreachable and
// retried, and the others are served (package documentation).
func (r *Registry) warnFileLimit(entries []*entry) {
	limit := r.fileLimit()
	need, most := int64(fileMargin), 0
	for _, e := range entries {
		per := filesPerTenant
		if c, ok := r.cfg.Drivers[e.driver].(FileCounter); ok && c.FilesPerTenant() > 0 {
			per = c.FilesPerTenant()
		}
		need += int64(per)
		most = max(most, per)
	}
	if limit < 0 || need <= limit {
		return
	}
	r.log.Warn("the limit on open files may be too low for every tenant's database: raise it (ulimit -n, LimitNOFILE)",
		"tenants", len(entries), "files_needed", need, "files_per_tenant", most, "limit", limit)
}
