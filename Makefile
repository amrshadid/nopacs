# The quality gates, one word each, run locally before each push.
#
#   make gates   the gates before each push; never the scale tests
#   make check   every gate below but test-scale, which runs on its own
#   make check-comments  no comment, document or script refers to a note
#                that is not in the repository (scripts/check/comments.py)
#   make fmt     gofmt -s; fails on any file it would change
#   make vet     go vet on 64 bits, on 32 bits and for Windows
#   make lint    golangci-lint with .golangci.yml
#   make vuln    govulncheck
#   make test    go test -race
#   make test-scale  the registry at 1,000 tenants, without the race detector
#   make build   static binaries for every target in bin/: nopacs (the command
#                line, an API client) and nopacs-server (the server and the
#                commands that touch the data directory with it stopped)
#   make size    the size of each binary make build left in bin/, both
#                programs, as a Markdown table
#
# Two more checks, which CI runs as steps of their own and make check does
# not, because each needs more than Go:
#
#   make check-seal  the sealed-secret format against Python's cryptography
#                    package (PYTHON=... names an interpreter that has it)
#   make fuzz        each fuzz target for FUZZTIME (10s unless given)
#   make check-master-sqlite  the SQLite master database read back by the
#                    sqlite3 tool (SQLITE3=... names one)
#   make check-e2e   the real program end to end, init to restart, with the
#                    real commands as processes
#   make check-tenant-sqlite  a tenant's SQLite database, created by the
#                    registry, read back by the sqlite3 tool
#   make check-master-postgres  the Postgres master database read back by
#                    psql and pg_dump (PSQL=... PG_DUMP=... name them; needs
#                    NOPACS_TEST_POSTGRES, the URL of a scratch database)
#   make check-tenant-postgres  a tenant on Postgres, created by the registry,
#                    read back by psql and pg_dump (same variables)
#   make test-386    the tests as linux/386 binaries, on a Linux x86-64 host
#   make prove-reception  the reception proven from outside: dcmtk and
#                    pynetdicom send, dcmdump, pydicom and sqlite3 check, kills
#                    at each step (DCMTK=dir; PROVE_S3=url adds a bucket)
#   make bench-gate  reception's speed against BENCH_BASE (origin/develop),
#                    failing on a slowdown past BENCH_MARGIN percent
#
# And everything CI runs, on one machine (the last section of this file):
#
#   make ci          every job of .github/workflows/checks.yml in one run, in
#                    an order that fails fast, ending with make prove. Needs
#                    docker (Postgres, S3, the proof), sqlite3, curl, openssl
#                    and python3; the pinned Go tools, grpcurl and the Python
#                    packages are installed into bin/ on first use. It runs on
#                    Linux and macOS, removes every container it started
#                    however it ends, and lists at the end what it could not
#                    run on this machine (on a Mac: the 32-bit tests, which
#                    are built and vetted instead, and the second-user test)
#   make outside     every outside check that needs a tool that is not Go;
#                    the two Postgres ones when a Postgres is up
#   make tools-python  the Python environment the outside checks need, in
#                    bin/python, from the hashed requirement files; once it
#                    exists, PYTHON defaults to its interpreter
#   make pg-up / pg-down  a throwaway Postgres in docker (PG_VERSION, on
#                    127.0.0.1:PG_PORT); pg-up prints NOPACS_TEST_POSTGRES
#   make s3-up / s3-down  the two S3 servers CI tests against, in docker;
#                    s3-up prints each one's NOPACS_TEST_S3
#   make test-postgres  the database drivers' tests, and the reception end to
#                    end, against that Postgres (or NOPACS_TEST_POSTGRES),
#                    failing if there is none
#   make test-s3     the S3 driver's tests, and the reception end to end,
#                    against each S3 server, failing if one is not up
#   make check-static  bin/nopacs-linux-amd64 and bin/nopacs-server-linux-amd64
#                    are static binaries built with CGO_ENABLED=0
#
# A target that needs docker fails with exit status 2, saying so, when the
# docker command does not answer.

GO      ?= go
BIN     := bin
MODULE  := github.com/amrshadid/nopacs

# Pinned, so a gate does not change under a pull request that changed nothing.
# golangci-lint's version is also in checks.yml; the two move together.
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.10.0

# The tools are installed into bin/tools in this checkout, not into GOPATH, so
# the pinned versions never replace the ones a person has installed. Each is in
# a directory named for its version, so changing the pin installs the new one.
# CI passes GOLANGCI_LINT=golangci-lint to use the binary its action installs.
TOOLS         := $(CURDIR)/$(BIN)/tools
GOLANGCI_LINT ?= $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)/golangci-lint
GOVULNCHECK   ?= $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)/govulncheck

# The Go files gofmt checks: all of them, except under directories whose names
# start with a dot (which the go command ignores too) and the build output.
GOFILES = $(shell find . -path './.*' -prune -o -path './$(BIN)' -prune -o -name '*.go' -print)

# The targets a release ships. Each is built with CGO_ENABLED=0: the binary must
# start on any system of its kind without a C library beside it. linux/386 is
# here because int is 32 bits there, and an overflow that fits everywhere else
# fails to build or misbehaves.
PLATFORMS := linux/amd64 linux/386 linux/arm64 darwin/arm64 windows/amd64

# VERSION=v1.2.3 stamps a version over the one in the build information.
VERSION ?=
LDFLAGS := -s -w $(if $(VERSION),-X $(MODULE)/internal/version.Version=$(VERSION))

.DEFAULT_GOAL := check
.PHONY: check check-comments fmt vet lint vuln test test-scale bench-events build size check-seal fuzz

check: check-comments fmt vet lint vuln test build size

.PHONY: gates
# The gates before each push, and never a scale test: the comments, gofmt,
# go vet on 64 bits, linux/386 and windows, lint, and the race tests of every
# package. The scale variables are emptied, so a value left in the shell does
# not turn a scale test on.
gates: check-comments check-docs check-doc-links fmt vet lint
	NOPACS_TEST_SCALE= NOPACS_TEST_ALL_SEEDS= NOPACS_SCALE_INSTANCES= $(GO) test -race -count=1 ./...

# The reference pages of docs/reference are written from the code that defines
# what they list, so they cannot say what the programs do not do. check-docs
# writes nothing and fails, naming the page, when a committed page differs; the
# metrics page starts a throwaway server on a short path under /tmp.
.PHONY: docs check-docs check-doc-links
docs:
	$(GO) run ./internal/testkit/cmd/refdocs

check-docs:
	$(GO) run ./internal/testkit/cmd/refdocs -check

# Every relative link and anchor in README.md and docs/ resolves; the checker is
# tried first on a tree of its own with known good and bad links.
check-doc-links:
	@command -v "$(PYTHON)" > /dev/null || { echo "check-doc-links: no interpreter $(PYTHON)" >&2; exit 1; }
	$(PYTHON) -I scripts/check/doc-links.py --self-test
	$(PYTHON) -I scripts/check/doc-links.py

# Every comment, document and script message says its reason itself; one that
# points at a note a reader of the repository cannot see explains nothing. The
# forms such a pointer takes are listed in the script, which reads every
# tracked file but gen/ and go.sum, with Python's standard library only.
# PYTHON is defined with proto-python below.
check-comments:
	@command -v "$(PYTHON)" > /dev/null || { echo "check-comments: no interpreter $(PYTHON)" >&2; exit 1; }
	$(PYTHON) -I scripts/check/comments.py

# gofmt's exit status is checked as well as its list: a file that does not
# parse is reported on standard error with status 2 and is not in the list, so
# looking at the list alone would pass it.
fmt:
	@files="$$(gofmt -s -l $(GOFILES))" || { echo "gofmt -s failed: a file above does not parse"; exit 1; }; \
	if [ -n "$$files" ]; then \
		echo "gofmt -s would change these files:"; echo "$$files"; \
		gofmt -s -d $$files; exit 1; \
	fi; \
	echo "gofmt -s: all $(words $(GOFILES)) Go files are formatted"

# GOOS=linux with GOARCH=386, because there is no 32-bit macOS: on a Mac the
# 386 vet would otherwise fail before it looked at the code.
vet:
	$(GO) vet ./...
	GOOS=linux GOARCH=386 $(GO) vet ./...
	GOOS=windows $(GO) vet ./...

# Linted once per system, because a file built only for one system
# (perm_windows.go, acl_darwin.go) is invisible to a run for another; CI runs
# this on Linux, so without the GOOS runs those files would never be linted.
lint: $(filter $(TOOLS)/%,$(GOLANGCI_LINT))
	$(GOLANGCI_LINT) version
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...
	GOOS=windows $(GOLANGCI_LINT) run ./...
	GOOS=darwin $(GOLANGCI_LINT) run ./...
	$(GOLANGCI_LINT) run --build-tags interrupt ./internal/interrupt/... ./internal/correct/... ./internal/rebuild/...
	$(GOLANGCI_LINT) run --build-tags clock ./internal/clock/...

vuln: $(filter $(TOOLS)/%,$(GOVULNCHECK))
	$(GOVULNCHECK) -version
	$(GOVULNCHECK) ./...

# -count=1: the tests always run. CI restores Go's build cache, which also
# holds test results, and without it every package whose code did not change
# reports "(cached)": a race or a timing fault in it would never be looked
# for again. Compiled code still comes from the cache.
test:
	$(GO) test -race -count=1 ./...

# The scale tests, which the everyday run skips unless NOPACS_TEST_SCALE is
# set: the registry at 1,000 tenants (TestScaleThousandTenants), the
# drivers' tests at scale, and the pipeline model under every seed
# (NOPACS_TEST_ALL_SEEDS). Without the race detector, which these tests
# skip: under it they measured the detector. -v so their numbers are in
# the log.
test-scale:
	NOPACS_TEST_SCALE=1 $(GO) test -count=1 -v -run '^TestScaleThousandTenants$$' ./internal/registry
	NOPACS_TEST_SCALE=1 NOPACS_TEST_ALL_SEEDS=1 $(GO) test -count=1 -v -timeout 30m -run '^(TestScale|TestPipelineModelAllSeeds$$)' ./internal/driver/sqlite
	NOPACS_TEST_SCALE=1 $(GO) test -count=1 -v -run '^TestScalePipelinePlans$$' ./internal/driver/postgres
	NOPACS_TEST_SCALE=1 $(GO) test -count=1 -v -timeout 30m -run '^TestScale' ./internal/receive

# Reception's speed held against a base commit (scripts/check/bench-gate.py):
# files a second from landing to verified for small slices on SQLite and a
# directory, the base's tree and this one measured in turns on the same
# machine; it fails when this one is slower by more than BENCH_MARGIN
# percent. BENCH_BASE is the commit to compare with.
BENCH_BASE   ?= origin/develop
BENCH_MARGIN ?= 25
BENCH_COUNT  ?= 5
.PHONY: bench-gate
bench-gate:
	python3 scripts/check/bench-gate.py --go "$(GO)" --base "$(BENCH_BASE)" --count $(BENCH_COUNT) --margin $(BENCH_MARGIN)

# The event feed's speed: a step with its event, a page read from a feed of
# ten million events (built once, about a minute), and what waking the
# watches costs. Not part of `check`: it takes minutes.
bench-events:
	$(GO) test -count=1 -run '^$$' -bench '^(BenchmarkReadEventsPage|BenchmarkReceptionStepEvent)$$' -benchtime 2s -timeout 30m ./internal/driver/sqlite
	$(GO) test -count=1 -run '^$$' -bench '^BenchmarkWake$$' ./internal/events

build:
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		for prog in nopacs nopacs-server; do \
			out=$(BIN)/$$prog-$$os-$$arch; \
			if [ "$$os" = windows ]; then out=$$out.exe; fi; \
			echo "CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags \"$(LDFLAGS)\" -o $$out ./cmd/$$prog"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/$$prog; \
		done; \
	done

# size reads what build made and does not depend on it: CI writes this target's
# output to the job summary, and a rebuild here would put build commands there
# in front of the table.
size:
	@ls $(BIN)/nopacs-* > /dev/null 2>&1 || { echo "no binaries in $(BIN)/; run make build first" >&2; exit 1; }
	@echo "| Binary | Bytes | MiB |"
	@echo "|---|---:|---:|"
	@for f in $(BIN)/nopacs-*; do \
		bytes=$$(wc -c < "$$f" | tr -d ' '); \
		echo "| $${f#$(BIN)/} | $$bytes | $$(awk "BEGIN { printf \"%.2f\", $$bytes / 1048576 }") |"; \
	done

# The outside check of internal/secret: scripts/check/seal.py opens what Go
# sealed and seals what Go then opens. -count=1 so a cached pass is never
# reported for a Python that changed. The test skips only when the variable is
# unset; this target always sets it, so here the check runs or fails, and an
# empty PYTHON or an interpreter that is not there fails before the test.
# PYTHON is the same variable proto-python uses, defined with it below.
check-seal:
	@test -n "$(PYTHON)" || { echo "check-seal: PYTHON is empty; name an interpreter that has the cryptography package" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-seal: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_SEAL_PYTHON="$(PYTHON)" $(GO) test -count=1 -v -run '^TestSealOpensInPython$$' ./internal/secret

# The outside check of internal/secret's backup format:
# scripts/check/backup-cipher.py derives the keys with hmac and hashlib,
# opens a backup Go wrote with the cryptography package, refuses it cut,
# reordered, changed and extended, and writes one that Go then opens.
.PHONY: check-backup-cipher check-auditchain check-record-mac
check-backup-cipher:
	@test -n "$(PYTHON)" || { echo "check-backup-cipher: PYTHON is empty; name an interpreter that has the cryptography package" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-backup-cipher: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_BACKUP_PYTHON="$(PYTHON)" $(GO) test -count=1 -v -run '^TestBackupOpensInPython$$' ./internal/secret

# The outside check of the correction record's MAC: scripts/check/record-mac.py
# derives the key with hmac and hashlib and computes the MAC of a record Go
# wrote, and refuses it for another tenant, changed, and without its MAC.
check-record-mac:
	@test -n "$(PYTHON)" || { echo "check-record-mac: PYTHON is empty; name a Python interpreter" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-record-mac: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_RECORD_MAC_PYTHON="$(PYTHON)" $(GO) test -count=1 -v -run '^Test(Hold)?RecordMACInPython$$' ./internal/core/record

# The outside check of internal/core/auditchain: scripts/check/auditchain.py
# recomputes the entry encoding and the chain with hashlib and has openssl
# verify each checkpoint's Ed25519 signature with pkeyutl -rawin. OPENSSL
# must be OpenSSL 3.0 or later; the macOS system LibreSSL has no Ed25519.
OPENSSL ?= openssl
check-auditchain:
	@test -n "$(PYTHON)" || { echo "check-auditchain: PYTHON is empty; name a Python interpreter" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-auditchain: no interpreter $(PYTHON)" >&2; exit 1; }
	@command -v "$(OPENSSL)" > /dev/null || { echo "check-auditchain: no openssl $(OPENSSL)" >&2; exit 1; }
	NOPACS_CHECK_AUDITCHAIN_PYTHON="$(PYTHON)" NOPACS_CHECK_AUDITCHAIN_OPENSSL="$(OPENSSL)" $(GO) test -count=1 -v -run '^TestChainChecksInPython$$' ./internal/core/auditchain
	NOPACS_CHECK_AUDITCHAIN_PYTHON="$(PYTHON)" NOPACS_CHECK_AUDITCHAIN_OPENSSL="$(OPENSSL)" $(GO) test -count=1 -v -run '^TestSealChecksOutside$$' ./internal/auditseal

# The outside checks of reading the archive, each the Go test that runs a
# script of scripts/check against what the server answers: the Metadata
# service against pydicom's to_json_dict, the Catalog service against what
# pydicom reads of the received files, each file's frame table against the
# fragments pydicom divides it into, QIDO-RS and WADO-RS from dicomweb-client
# against pydicom, and STOW-RS from dicomweb-client in a real server process.
# As for check-seal, -count=1 so a cached pass is never reported, and the
# variable is always set, so the check runs or fails. PYTHON needs pydicom,
# and for check-dicomweb and check-stow dicomweb-client too
# (docs/develop/testing.md). PYDICOM_CORPUS=<pydicom's test_files directory> adds
# that corpus's files where the test takes them.
PYDICOM_CORPUS ?=
CHECK_READING_ENV = $(if $(PYDICOM_CORPUS),NOPACS_PYDICOM_CORPUS="$(PYDICOM_CORPUS)")

.PHONY: check-metadata check-catalog check-frames check-dicomweb check-stow check-deid check-j2k check-send

check-metadata:
	@test -n "$(PYTHON)" || { echo "check-metadata: PYTHON is empty; name an interpreter that has pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-metadata: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_METADATA_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^TestMetadataEqualsPydicom$$' ./internal/api

check-catalog:
	@test -n "$(PYTHON)" || { echo "check-catalog: PYTHON is empty; name an interpreter that has pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-catalog: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_CATALOG_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^TestCatalogEqualsPydicom$$' ./internal/api

check-frames:
	@test -n "$(PYTHON)" || { echo "check-frames: PYTHON is empty; name an interpreter that has pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-frames: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_FRAMES_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^TestTableEqualsPydicom$$' ./internal/receive/frames

check-dicomweb:
	@test -n "$(PYTHON)" || { echo "check-dicomweb: PYTHON is empty; name an interpreter that has dicomweb-client and pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-dicomweb: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_DICOMWEB_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^TestDICOMwebWithDicomwebClient$$' ./internal/api

# The de-identification engine against pydicom: every file the engine
# de-identifies from the corpus is read by pydicom and searched for each value
# the test profile removes.
check-deid:
	@test -n "$(PYTHON)" || { echo "check-deid: PYTHON is empty; name an interpreter that has pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-deid: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_DEID_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^(TestCorpusFilesAreDeidentified|TestValuePatternsAgreeWithPydicom)$$' ./internal/deid/engine
	NOPACS_CHECK_DEID_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^TestAgreesWithPydicom$$' ./internal/deid/check

# The JPEG 2000 walk against OpenJPEG (OPENJPEG=<dir> holding opj_compress,
# opj_decompress and opj_dump): what the walk leaves decodes to the samples
# received, and pydicom decodes the de-identified files alike.
check-j2k:
	@test -n "$(OPENJPEG)" || { echo "check-j2k: OPENJPEG is empty; name the directory holding opj_compress, opj_decompress and opj_dump" >&2; exit 1; }
	NOPACS_CHECK_J2K_OPENJPEG="$(OPENJPEG)" NOPACS_CHECK_DEID_PYTHON="$(PYTHON)" $(CHECK_READING_ENV) $(GO) test -count=1 -v -run '^(TestJ2KRewriteDecodesAlikeWithOpenJPEG|TestJ2KFreeWordsDecodeAlikeWithOpenJPEG)$$' ./internal/deid/engine

check-stow:
	@test -n "$(PYTHON)" || { echo "check-stow: PYTHON is empty; name an interpreter that has dicomweb-client and pydicom" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-stow: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_CHECK_STOW_PYTHON="$(PYTHON)" $(GO) test -count=1 -v -run '^TestSTOWWithDicomwebClient$$' ./internal/servecmd

# The senders against dcmtk's storescp (+B) and a pynetdicom peer: each data
# set they write is the archived one, byte for byte (DCMTK=<dir>, PYTHON=...).
check-send:
	@command -v "$(PYTHON)" > /dev/null || { echo "check-send: no interpreter $(PYTHON)" >&2; exit 1; }
	NOPACS_REQUIRE_DCMTK=1 NOPACS_DCMTK="$(DCMTK)" NOPACS_PYTHON="$(PYTHON)" $(GO) test -count=1 -v -run '^TestOutside' ./internal/send

# The outside check of the SQLite master database: the driver writes the
# contract suite's sample, and the sqlite3 tool (SQLITE3=... names one) reads
# it back, checks it, and searches it for the sample key's secret.
SQLITE3 ?= sqlite3

.PHONY: check-master-sqlite test-386
check-master-sqlite:
	SQLITE3="$(SQLITE3)" GO="$(GO)" bash scripts/check/master-sqlite.sh

# The outside check of the server: curl, grpcurl (GRPCURL=... names one) and
# Python's grpcio (PYTHON=... names an interpreter with
# scripts/requirements/check-server.txt) against a real server process with
# the stand-in handlers and against the real program with the real handlers,
# over TLS on loopback and over the local socket.
GRPCURL ?= grpcurl

.PHONY: check-server
check-server:
	GO="$(GO)" PYTHON="$(PYTHON)" GRPCURL="$(GRPCURL)" bash scripts/check/server.sh

# The real program end to end: init, serve with the local socket and TLS,
# and the real commands as processes, a stop by signal and a restart
# (TestRealServerEndToEnd). It starts some fifty processes, so it runs here
# and in CI's server job, not in every go test.
.PHONY: check-e2e
check-e2e:
	NOPACS_E2E=1 $(GO) test -race -count=1 -v -run '^TestRealServerEndToEnd$$' ./cmd/nopacs-server

# The proof of this version (scripts/prove/run.sh): the binary built as make
# build builds it, started for real, and every claim of this version shown by
# tools that are not NoPACs (curl, grpcurl, openssl, sqlite3, psql and
# pg_dump in a Postgres container, Python's grpcio, and for reading the
# archive dcmtk's storescu, pydicom and dicomweb-client), each as a named
# check, with a table of the results and what they measured. It needs docker
# for Postgres, DCMTK= naming dcmtk's directory, and a PYTHON with
# scripts/requirements/check-server.txt and prove-reading.txt (pydicom 3, so
# not the environment of prove-reception, which pins pydicom 2). CI runs it
# on Linux with NOPACS_PROVE_STRICT=1, where a skipped check fails.
.PHONY: prove
prove:
	GO="$(GO)" PYTHON="$(PYTHON)" GRPCURL="$(GRPCURL)" SQLITE3="$(SQLITE3)" DCMTK="$(DCMTK)" bash scripts/prove/run.sh

# The proof of reception (scripts/prove/reception.py): the real program
# built, served and set up with its own commands; dcmtk's storescu and
# pynetdicom send public studies split, sent again, corrected, cut short and
# from strangers; dcmdump, pydicom, hashlib and sqlite3 check what was
# archived, quarantined and counted; the server is killed with signal 9 while
# receiving, in the quiet period and while archiving, and nothing is lost or
# archived twice; a storage nearly full, the status and the event stream over
# HTTPS, and streams that stop reading. Needs dcmtk (DCMTK=dir, since
# pynetdicom installs programs of the same names), openssl, and a PYTHON with
# scripts/requirements/prove-reception.txt. PROVE_CRASH_ROUNDS kills per step.
# PROVE_S3, a URL as make s3-up prints it, adds a tenant on that S3 server,
# read back by GETs the proof signs itself; PROVE_POSTGRES (a server's
# address whose login may make databases) with PROVE_PSQL (the psql command
# that reaches it) adds a tenant on Postgres whose catalog psql reads.
# Without them those checks skip.
DCMTK              ?=
OPENJPEG           ?=
PROVE_CRASH_ROUNDS ?= 2
PROVE_S3           ?=
PROVE_POSTGRES     ?=
PROVE_PSQL         ?=
.PHONY: prove-reception
prove-reception:
	"$(PYTHON)" scripts/prove/reception.py --go "$(GO)" --dcmtk "$(DCMTK)" --crash-rounds $(PROVE_CRASH_ROUNDS) \
		--s3 "$(PROVE_S3)" --postgres "$(PROVE_POSTGRES)" --psql "$(PROVE_PSQL)"

# The proof of sending (scripts/prove/sending.py): the real program built,
# served and set up with its own commands; public and synthetic studies
# archived from dcmtk's storescu are sent to an Orthanc by DIMSE and by
# STOW-RS, and Orthanc's REST API and pydicom check each instance against the
# archived file; the server stopped by SIGTERM and by SIGKILL midway, a job
# canceled and retried, a receiver's forward-to, a pynetdicom peer refusing
# some instances, and a de-identifying receiver whose data reaches Orthanc
# with no original value; then instances per second by each way. Needs an
# Orthanc with its DICOMweb plugin that holds nothing (PROVE_ORTHANC its REST
# API, PROVE_ORTHANC_DIMSE its AET@HOST:PORT), dcmtk (DCMTK=dir), and a PYTHON
# with scripts/requirements/prove-reception.txt. PROVE_ORTHANC_LOGS, a command
# that prints Orthanc's log, counts what was sent twice; without it that
# check skips. PROVE_SEND_MANY is the size of the session stopped midway.
PROVE_ORTHANC       ?= http://127.0.0.1:8042
PROVE_ORTHANC_DIMSE ?= ORTHANC@127.0.0.1:4242
PROVE_ORTHANC_LOGS  ?=
PROVE_SEND_MANY     ?= 400
.PHONY: prove-sending
prove-sending:
	"$(PYTHON)" scripts/prove/sending.py --go "$(GO)" --dcmtk "$(DCMTK)" --orthanc "$(PROVE_ORTHANC)" \
		--orthanc-dimse "$(PROVE_ORTHANC_DIMSE)" --orthanc-logs "$(PROVE_ORTHANC_LOGS)" --many $(PROVE_SEND_MANY) \
		$(if $(PROVE_POSTGRES),--postgres "$(PROVE_POSTGRES)" --psql "$(PROVE_PSQL)")

# The proof of de-identification (scripts/prove/deid.py): the real program
# built, served and set up with its own commands; public corpus files and
# files built with pydicom, with identifying values planted where they hide
# (names in every script, dates written many ways, private elements, JPEG
# and JPEG 2000 comments, burned-in pixels), go through de-identifying
# receivers. pydicom, dcmdump, dcmdjpeg, sqlite3 and a byte search check that
# none survives, that every failure holds the file with its reason, that
# releases archive it, and that the server killed with signal 9 loses
# nothing. PYDICOM_CORPUS names pydicom's whole test_files directory (the
# installed pydicom lacks some files). The whole proof runs several minutes;
# PROVE_DEID_ONLY (groups: faults rules trial check crash numbers more memory
# forward nowhere) runs a part. PROVE_DEID_ORTHANC=1 adds the last step, a
# session read back from the empty Orthanc of PROVE_ORTHANC.
PROVE_DEID_ONLY    ?=
PROVE_DEID_ORTHANC ?=
.PHONY: prove-deid
prove-deid:
	"$(PYTHON)" scripts/prove/deid.py --go "$(GO)" --dcmtk "$(DCMTK)" --only "$(PROVE_DEID_ONLY)" \
		$(if $(PYDICOM_CORPUS),--pydicom-corpus "$(PYDICOM_CORPUS)") \
		$(if $(PROVE_DEID_ORTHANC),--orthanc "$(PROVE_ORTHANC)" --orthanc-dimse "$(PROVE_ORTHANC_DIMSE)")

# The viewer harness (scripts/prove/viewer.py): the real program built, served
# and set up with its own commands, public studies (CT, MR, multi-frame, JPEG
# baseline, JPEG 2000, RLE) archived from dcmtk's storescu; OHIF Viewer (the
# ohif/app image, pinned) and headless Chromium (the Playwright image, pinned)
# run in containers, open each study, and the script prints whether each
# picture is blank, every request that failed (path, status, Accept) and the
# most reads the viewer kept open at once. Needs Docker (PROVE_DOCKER_HOST, a
# DOCKER_HOST value), dcmtk (DCMTK=dir), and a PYTHON with
# scripts/requirements/prove-reading.txt. PROVE_VIEWER_SHOTS names a directory
# for the screenshots and result.json.
PROVE_DOCKER_HOST   ?=
PROVE_VIEWER_SHOTS  ?=
.PHONY: prove-viewer
prove-viewer:
	"$(PYTHON)" scripts/prove/viewer.py --go "$(GO)" --dcmtk "$(DCMTK)" \
		$(if $(PROVE_DOCKER_HOST),--docker-host "$(PROVE_DOCKER_HOST)") \
		$(if $(PROVE_VIEWER_SHOTS),--keep-shots "$(PROVE_VIEWER_SHOTS)")

# The proof of importing a directory (scripts/prove/importing.py): the real
# program built, served and set up with its own commands; trees of two layouts
# built from the public corpus with traps planted (a link, two names of one
# file, files that are not DICOM, a folder wider than import.max-dir-entries, a
# path over 2048 bytes, a folder named "ct:9", a file outside the layout, an
# unmapped session, a file over the largest file, an instance in two folders)
# are imported through the job commands; the counts and both identities, every
# skip listed, the archive read back with pydicom and SHA-256 equal to the
# tree, the tree unchanged, a second run that holds everything, roots refused,
# a reader that sees no path, a de-identifying receiver whose archive carries
# no value of the tree with an SR's references resolving after it, and a
# server killed with signal 9 midway ending as an uninterrupted import does.
# No container and no dcmtk; PROVE_IMPORT_ONLY (groups: tree deid kill) runs
# a part.
PROVE_IMPORT_ONLY ?=
.PHONY: prove-import
prove-import:
	"$(PYTHON)" scripts/prove/importing.py --go "$(GO)" --only "$(PROVE_IMPORT_ONLY)" \
		$(if $(PYDICOM_CORPUS),--pydicom-corpus "$(PYDICOM_CORPUS)")

# The proof that the archive is kept whole (scripts/prove/whole.py): the real
# program built, served and set up with its own commands; studies built from
# the public corpus archived with dcmtk's storescu (as received and through a
# de-identifying receiver); a relabel and a replace checked on the storage
# directory (keys moved, SHA-256 equal, old keys gone after the purge, the
# .r2.dcm object read by pydicom, the ETag changed under curl) and the catalog
# made again from the storages by a restore compared with catalog_dump.py
# (diff); a stored byte flipped, an object removed and one added, found by a
# scrub that changes nothing, with curl refused the damaged instance; a deleted
# subject gone from the storage and the catalog, its audit entry, a re-send
# quarantined as was_deleted and arriving again once allowed; a backup restored
# into a destroyed data directory (a changed backup restores nothing, keys
# suspended, a later delete held as unresolved); a correction record written by
# hand not applied by a rebuild. Later groups: purge dispositions on versioned
# and object-locked buckets, a restore onto a fresh host and its time, the event
# history after a restore, one writer per storage across a move, a send meeting
# corrections, ResolveInstances, references after corrections, provenance of
# received bytes, what a client sees of a damaged instance, objects not online
# and the purge clock. PROVE_WHOLE_ONLY (groups: rebuild scrub delete restore
# records versions fresh cursor writer jobs resolve references provenance client
# room) runs a part. versions and room start an S3 server (S3_SILO_IMAGE) in a
# container named for the run and remove it after. Needs dcmtk (DCMTK=dir),
# openssl, docker for those two, and a PYTHON with
# scripts/requirements/prove-whole.txt.
PROVE_WHOLE_ONLY ?=
.PHONY: prove-whole
prove-whole:
	"$(PYTHON)" scripts/prove/whole.py --go "$(GO)" --dcmtk "$(DCMTK)" --python "$(PYTHON)" --only "$(PROVE_WHOLE_ONLY)" \
		--docker "$(DOCKER)" --s3-image "$(S3_SILO_IMAGE)" $(if $(PYDICOM_CORPUS),--pydicom-corpus "$(PYDICOM_CORPUS)")

# The interruption harness: the server built with the build tag "interrupt"
# killed at each named durable step of a correction (delete, relabel, replace,
# their purges and an undo) and of a rebuild (replace and compare), with
# downloads, an If-Range resume, DICOMweb, ReadFiles, catalog reads and a send
# to a pynetdicom peer running across the kills, judged after each restart by
# curl, hashlib, sqlite3, pydicom and catalog_dump.py
# (scripts/prove/interrupt.py). No container. PROVE_INTERRUPT_ONLY (groups:
# replace relabel rebuild delete) runs a part.
PROVE_INTERRUPT_ONLY ?=
.PHONY: prove-interrupt
prove-interrupt:
	"$(PYTHON)" scripts/prove/interrupt.py --go "$(GO)" --dcmtk "$(DCMTK)" --python "$(PYTHON)" --only "$(PROVE_INTERRUPT_ONLY)" \
		$(if $(PYDICOM_CORPUS),--pydicom-corpus "$(PYDICOM_CORPUS)")

# The outside check of an audit export through the real program: a server
# seals its logs, the commands export them and write the public key, and
# scripts/prove/audit_verify.py (hashlib and openssl, nothing of NoPACs')
# accepts each export and refuses a changed one, naming the seq.
.PHONY: check-audit-export
check-audit-export:
	@test -n "$(PYTHON)" || { echo "check-audit-export: PYTHON is empty; name a Python interpreter" >&2; exit 1; }
	@command -v "$(PYTHON)" > /dev/null || { echo "check-audit-export: no interpreter $(PYTHON)" >&2; exit 1; }
	@command -v "$(OPENSSL)" > /dev/null || { echo "check-audit-export: no openssl $(OPENSSL)" >&2; exit 1; }
	NOPACS_CHECK_AUDIT_EXPORT_PYTHON="$(PYTHON)" NOPACS_CHECK_AUDIT_EXPORT_OPENSSL="$(OPENSSL)" $(GO) test -count=1 -v -run '^TestAuditExportChecksOutside$$' ./cmd/nopacs-server

# The proof's outside tools check themselves against audit exports, catalogs
# and backups they build and then break: audit_verify (an exported audit log's
# chain and signatures, against public keys the auditor gives; OPENSSL 3.0 or
# later), backup_open (a backup opened with the key file, plaintext staged
# until every chunk opens; PYTHON with the cryptography package) and
# catalog_dump (a tenant catalog as canonical JSON; with NOPACS_TEST_POSTGRES
# and PSQL it also compares a Postgres catalog with SQLite's).
.PHONY: check-prove-tools
check-prove-tools:
	OPENSSL="$(OPENSSL)" "$(PYTHON)" scripts/prove/audit_verify.py --self-test
	"$(PYTHON)" scripts/prove/backup_open.py --self-test
	"$(PYTHON)" scripts/prove/catalog_dump.py --self-test
	"$(PYTHON)" scripts/prove/s3_stub.py --self-test
	"$(PYTHON)" scripts/prove/tree_compare.py --self-test
	GO="$(GO)" "$(PYTHON)" scripts/prove/clock_offset.py --self-test

# Sending's speed: a scan of 64 small instances to an in-process DIMSE and
# STOW-RS peer, three runs of twenty jobs each.
.PHONY: bench-send
bench-send:
	$(GO) test -count=3 -run '^$$' -bench '^BenchmarkSendScan$$' -benchtime 20x ./internal/send


# The outside check of a tenant's SQLite database: the registry creates a
# tenant on SQLite, and the sqlite3 tool reads its file and the master's row.
.PHONY: check-tenant-sqlite
check-tenant-sqlite:
	SQLITE3="$(SQLITE3)" GO="$(GO)" bash scripts/check/tenant-sqlite.sh

# The outside check of the Postgres master database: the driver writes the
# contract suite's sample into a schema of the database NOPACS_TEST_POSTGRES
# names, and psql and pg_dump (the commands in PSQL and PG_DUMP, which may run
# in a container so their version equals the server's) read it back.
PSQL    ?= psql
PG_DUMP ?= pg_dump

.PHONY: check-master-postgres check-tenant-postgres
check-master-postgres:
	PSQL="$(PSQL)" PG_DUMP="$(PG_DUMP)" GO="$(GO)" bash scripts/check/master-postgres.sh

# The same for a tenant on Postgres: the registry creates it, the tenant
# suite's sample is written, and psql and pg_dump read the tenant's schema and
# the master's.
check-tenant-postgres:
	PSQL="$(PSQL)" PG_DUMP="$(PG_DUMP)" GO="$(GO)" bash scripts/check/tenant-postgres.sh

# The tests on 32 bits, where int is 32 bits and the SQLite library is a
# different translation of SQLite's C. Without -race, which 386 lacks. It runs
# natively on a Linux x86-64 machine (CI); macOS cannot run 386 binaries.
test-386:
	GOOS=linux GOARCH=386 CGO_ENABLED=0 $(GO) test -count=1 ./...

# Every fuzz target, as package:function. go test fuzzes one target at a
# time, so each is a run of its own; -run '^$$' keeps the other tests out.
FUZZTIME     ?= 10s
FUZZ_TARGETS := \
	internal/access:FuzzCheck \
	internal/access:FuzzCheckSQLite \
	internal/secret:FuzzOpen \
	internal/secret:FuzzBackupReader \
	internal/core/importpath:FuzzCompare \
	internal/core/importpath:FuzzCheck \
	internal/core/importpath:FuzzResolve \
	internal/dirimport/mapping:FuzzParse \
	internal/secret:FuzzParseAPIKey \
	internal/secret:FuzzParseKeyFile \
	internal/core/settings:FuzzParsePorts \
	internal/core/settings:FuzzParseSize \
	internal/core/settings:FuzzParseReserve \
	internal/core/settings:FuzzParseDirectoryRoots \
	internal/core/settings:FuzzParseS3Endpoints \
	internal/core/settings:FuzzParseBindAddresses \
	internal/core/settings:FuzzParseHeaderLimit

fuzz:
	@set -e; for target in $(FUZZ_TARGETS); do \
		pkg=$${target%%:*}; fn=$${target#*:}; \
		echo "$(GO) test -run '^$$' -fuzz ^$$fn\$$ -fuzztime $(FUZZTIME) ./$$pkg"; \
		$(GO) test -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) ./$$pkg; \
	done

# govulncheck needs a newer Go to build than this module needs, so the installs
# may fetch that toolchain (GOTOOLCHAIN=auto). The scan itself runs under the go
# command on PATH, whose standard library is the one checked.
$(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)/golangci-lint:
	GOBIN=$(@D) GOTOOLCHAIN=auto $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)/govulncheck:
	GOBIN=$(@D) GOTOOLCHAIN=auto $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# ---------------------------------------------------------------------------
# The API contract: the protobuf files in api/proto and the Go code generated
# from them into gen/.
#
#   make proto           regenerate gen/ from api/proto
#   make proto-lint      buf lint with the rules in buf.yaml
#   make proto-breaking  buf breaking against the commit where HEAD and
#                        PROTO_BASE (a git revision) meet
#   make proto-check     regenerate into a scratch directory and fail on any
#                        difference from the committed gen/
#   make proto-python    compile the proto files with Python's grpcio-tools,
#                        a second toolchain, and list the services; needs a
#                        PYTHON with scripts/requirements/proto.txt installed,
#                        so it is not part of make check
#
# buf and both plugins are pinned and installed into bin/tools like the tools
# above. buf is the release binary, checked against the SHA-256 committed in
# scripts/tools/buf.sha256 (building it from source took minutes in CI); the
# plugins are small and built with go install. buf runs with local plugins
# only: no Buf Schema Registry account, no remote plugin, no network at
# generation time.
BUF_VERSION                   := v1.75.0
PROTOC_GEN_GO_VERSION         := v1.36.12
PROTOC_GEN_CONNECT_GO_VERSION := v2.0.2

BUF                   := $(TOOLS)/buf-$(BUF_VERSION)/buf
PROTOC_GEN_GO         := $(TOOLS)/protoc-gen-go-$(PROTOC_GEN_GO_VERSION)/protoc-gen-go
PROTOC_GEN_CONNECT_GO := $(TOOLS)/protoc-gen-connect-go-$(PROTOC_GEN_CONNECT_GO_VERSION)/protoc-gen-connect-go

# buf.gen.yaml names the plugins without a path; the pinned ones are put first
# on PATH, so a plugin of another version installed elsewhere is never used.
PROTO_PATH := $(dir $(PROTOC_GEN_GO)):$(dir $(PROTOC_GEN_CONNECT_GO)):$(PATH)

# The branch the contract is compared with. The check compares with the commit
# where HEAD and this branch meet, not with its tip, so a branch that is behind
# does not see what was added since as deleted. CI passes the pull request's
# base branch, or the commit before a push.
PROTO_BASE ?= origin/develop

# Scratch space for the base's files and for regenerated code. Its name starts
# with "_", so the go command ignores it: regenerated Go code left here would
# otherwise be a package of the module, built and tested by ./... It is
# removed after each use.
PROTO_TMP := $(BIN)/_proto

# The interpreter of the outside checks: the one make tools-python made, once
# it has, so a check never runs with packages other than the pinned ones;
# otherwise python3 from PATH, as before that environment existed.
PYTHON ?= $(if $(wildcard $(PYTHON_STAMP)),$(PYTHON_ENV_PYTHON),python3)

.PHONY: proto proto-lint proto-breaking proto-check proto-python

check: proto-lint proto-breaking proto-check

proto-python:
	$(PYTHON) scripts/check/proto-python.py api/proto

# The generated Python client: its checked-in code must equal a fresh
# generation from the contract, and its tests run it against the real server.
# PYTHON needs scripts/requirements/client-python.txt installed, and the client
# installed with: pip install --no-deps --no-build-isolation ./clients/python
.PHONY: client-python-check client-python-test
client-python-check:
	$(PYTHON) clients/python/generate.py --check

client-python-test:
	$(PYTHON) -m pytest clients/python/tests

# The generated TypeScript client, the same two checks. Its packages come from
# its own lockfile, so each target installs them with npm ci first, without
# running any package's install script. The tests build and start the server,
# so they need Go; the TLS test needs openssl. The advisory audit
# (client-ts-audit) depends on the advisory database of the day, so it is not
# part of the check.
.PHONY: client-ts client-ts-check client-ts-test client-ts-audit
client-ts:
	cd clients/typescript && npm ci --ignore-scripts --no-audit --no-fund && npm run generate

client-ts-check:
	cd clients/typescript && npm ci --ignore-scripts --no-audit --no-fund && npm run generate:check && npm run typecheck

client-ts-test:
	cd clients/typescript && npm ci --ignore-scripts --no-audit --no-fund && npm test

client-ts-audit:
	cd clients/typescript && npm audit --omit=dev

proto: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_CONNECT_GO)
	@echo "buf generate (protoc-gen-go $(PROTOC_GEN_GO_VERSION), protoc-gen-connect-go $(PROTOC_GEN_CONNECT_GO_VERSION))"
	@PATH="$(PROTO_PATH)" $(BUF) generate

proto-lint: $(BUF)
	$(BUF) --version
	$(BUF) lint

# A base with no proto files has no contract to break; the check says so and
# passes rather than comparing against nothing. The base is read with git
# archive, so the check works the same in a clone, a worktree and CI.
proto-breaking: $(BUF)
	@set -e; base="$(PROTO_BASE)"; \
	git rev-parse --verify --quiet "$$base^{commit}" > /dev/null || { echo "proto-breaking: $$base is not a known revision; fetch it first" >&2; exit 1; }; \
	mb="$$(git merge-base HEAD "$$base")" || { echo "proto-breaking: HEAD and $$base have no commit in common" >&2; exit 1; }; \
	echo "proto-breaking: comparing with $$mb, where HEAD and $$base meet"; \
	if ! git ls-tree -r --name-only "$$mb" -- api/proto | grep '\.proto$$' > /dev/null; then \
		echo "proto-breaking: skipped, $$mb has no proto files under api/proto"; exit 0; \
	fi; \
	rm -rf $(PROTO_TMP); mkdir -p $(PROTO_TMP)/base; \
	git archive "$$mb" -- buf.yaml api/proto | tar -x -C $(PROTO_TMP)/base; \
	if [ -f api/renamed.txt ]; then \
		grep -v '^#' api/renamed.txt | while read -r old new; do \
			[ -n "$$old" ] || continue; \
			from="$(PROTO_TMP)/base/api/proto/$$(echo "$$old" | tr . /)"; \
			to="$(PROTO_TMP)/base/api/proto/$$(echo "$$new" | tr . /)"; \
			[ -d "$$from" ] || continue; \
			echo "proto-breaking: reading the base's $$old as $$new (api/renamed.txt)"; \
			mkdir -p "$$(dirname "$$to")"; mv "$$from" "$$to"; \
			OLD="$$old" NEW="$$new" OLDPATH="$$(echo "$$old" | tr . /)" NEWPATH="$$(echo "$$new" | tr . /)" \
				perl -pi -e 's/\b\Q$$ENV{OLD}\E\b/$$ENV{NEW}/g; s{"\Q$$ENV{OLDPATH}\E/}{"$$ENV{NEWPATH}/}g' $$(find "$(PROTO_TMP)/base/api/proto" -name '*.proto'); \
		done; \
	fi; \
	status=0; $(BUF) breaking --against $(PROTO_TMP)/base || status=$$?; \
	rm -rf $(PROTO_TMP); \
	if [ $$status -ne 0 ]; then echo "proto-breaking: the contract breaks what $$mb published" >&2; exit $$status; fi; \
	echo "proto-breaking: no breaking change"

proto-check: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_CONNECT_GO)
	@set -e; rm -rf $(PROTO_TMP); mkdir -p $(PROTO_TMP); \
	PATH="$(PROTO_PATH)" $(BUF) generate -o $(PROTO_TMP); \
	status=0; diff -r gen $(PROTO_TMP)/gen || status=$$?; \
	rm -rf $(PROTO_TMP); \
	if [ $$status -ne 0 ]; then \
		echo "proto-check: gen/ is not what the proto files generate; run make proto and commit the result" >&2; exit 1; \
	fi; \
	echo "proto-check: gen/ matches the proto files"

$(BUF):
	scripts/tools/install-buf.sh $(BUF_VERSION) $@

$(PROTOC_GEN_GO):
	GOBIN=$(@D) GOTOOLCHAIN=auto $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(PROTOC_GEN_CONNECT_GO):
	GOBIN=$(@D) GOTOOLCHAIN=auto $(GO) install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)

# ---------------------------------------------------------------------------
# The command line (internal/cli): two checks by tools that are not NoPACs.
#
#   make check-json        every command with --json against the in-process
#                          server, each output parsed by Python's json module
#                          (scripts/check/cli-json.py); needs python3, nothing else
#   make check-completion  the bash completion script the binary generates,
#                          parsed by bash -n
#
# Like check-seal they are steps of their own in CI, not part of make check.
# check-json always sets the variable that makes the Go test write the
# documents, and the script fails when one is missing, so the Go test being
# skipped cannot pass.
.PHONY: check-json check-completion

check-json:
	@command -v "$(PYTHON)" > /dev/null || { echo "check-json: no interpreter $(PYTHON)" >&2; exit 1; }
	@set -e; dir="$$(mktemp -d)"; \
	NOPACS_CHECK_JSON_DIR="$$dir" $(GO) test -count=1 -run '^TestWriteJSONOutputs$$' ./internal/cli; \
	status=0; $(PYTHON) scripts/check/cli-json.py "$$dir" || status=$$?; \
	rm -f "$$dir"/*.json; rmdir "$$dir"; exit $$status

check-completion:
	@command -v bash > /dev/null || { echo "check-completion: bash is not installed" >&2; exit 1; }
	@set -e; out="$$(mktemp)"; \
	$(GO) run ./cmd/nopacs completion bash > "$$out"; \
	status=0; \
	test -s "$$out" || { echo "check-completion: the script is empty" >&2; status=1; }; \
	[ $$status -ne 0 ] || grep -q '_nopacs' "$$out" || { echo "check-completion: the script does not define _nopacs" >&2; status=1; }; \
	[ $$status -ne 0 ] || bash -n "$$out" || status=$$?; \
	[ $$status -ne 0 ] || echo "check-completion: bash -n accepts the completion script ($$(wc -l < "$$out" | tr -d ' ') lines)"; \
	rm -f "$$out"; exit $$status

# ---------------------------------------------------------------------------
# The interactive program (internal/tui): an outside check by Python's pty
# module, which is not NoPACs' code.
#
#   make check-tui   builds the binary and a test helper that serves the
#                    in-process fake server on a temporary socket, then runs
#                    the binary under a pseudo-terminal (scripts/check/tui.py):
#                    keys pressed, the screen read, quit, a stop signal, and
#                    the terminal checked to have been restored.
#
# Like check-json it is a step of its own in CI. The helper is built from
# internal/testkit/cmd/ptyserve into a temporary directory: it is test support, it is
# not part of "make build", and it is never in the binary. Linux and macOS only
# (Windows has no pty module).
.PHONY: check-tui

check-tui:
	@command -v "$(PYTHON)" > /dev/null || { echo "check-tui: no interpreter $(PYTHON)" >&2; exit 1; }
	@set -e; dir="$$(mktemp -d)"; \
	CGO_ENABLED=0 $(GO) build -o "$$dir/nopacs" ./cmd/nopacs; \
	CGO_ENABLED=0 $(GO) build -o "$$dir/ptyserve" ./internal/testkit/cmd/ptyserve; \
	status=0; $(PYTHON) scripts/check/tui.py --binary "$$dir/nopacs" --helper "$$dir/ptyserve" || status=$$?; \
	rm -f "$$dir/nopacs" "$$dir/ptyserve"; rmdir "$$dir"; exit $$status

# ---------------------------------------------------------------------------
# Everything CI runs, on one machine: the Python tools, the services in
# docker, and make ci, which runs each job of .github/workflows/checks.yml in
# turn. The targets are listed at the top of this file.

# The Python environment of the outside checks: a virtual environment (the
# system's Python may refuse pip installs, PEP 668) with every package of the
# requirement files at its pinned version and hash, so a file on the package
# index that changed is refused. Wheels only, as CI installs them. Made once:
# the stamp is named for the list of files and is older than none of them, so
# changing a file or the list makes it again. PYTHON3 is the interpreter that
# makes it.
PYTHON3             ?= python3
PYTHON_ENV          ?= $(BIN)/python
PYTHON_REQUIREMENTS ?= scripts/requirements/check-seal.txt scripts/requirements/check-server.txt scripts/requirements/proto.txt scripts/requirements/prove-reception.txt
PYTHON_ENV_PYTHON   := $(abspath $(PYTHON_ENV))/bin/python
PYTHON_STAMP        := $(PYTHON_ENV)/.installed-$(shell printf '%s' '$(PYTHON_REQUIREMENTS)' | cksum | cut -d ' ' -f 1)

.PHONY: tools-python
tools-python: $(PYTHON_STAMP)

$(PYTHON_STAMP): $(PYTHON_REQUIREMENTS)
	@test -n "$(PYTHON_ENV)" || { echo "tools-python: PYTHON_ENV is empty" >&2; exit 2; }
	@command -v "$(PYTHON3)" > /dev/null || { echo "tools-python: no interpreter $(PYTHON3); PYTHON3=... names one" >&2; exit 2; }
	rm -rf "$(PYTHON_ENV)"
	"$(PYTHON3)" -m venv "$(PYTHON_ENV)"
	"$(PYTHON_ENV)/bin/pip" install --quiet --disable-pip-version-check --require-hashes --only-binary=:all: $(addprefix -r ,$(PYTHON_REQUIREMENTS))
	@"$(PYTHON_ENV)/bin/python" --version
	@touch "$@"

# grpcurl from its release, checked against scripts/tools/grpcurl.sha256, for the
# outside checks of the server when GRPCURL is not given. Keep in step with
# the version checks.yml installs.
GRPCURL_VERSION := v1.9.4
GRPCURL_TOOL    := $(TOOLS)/grpcurl-$(GRPCURL_VERSION)/grpcurl

$(GRPCURL_TOOL):
	bash scripts/tools/install-grpcurl.sh $(GRPCURL_VERSION) $@

# What the outside checks run with: what was given on the command line or in
# the environment, or else the pinned tools above, which are then installed
# first.
CHECK_PYTHON  = $(if $(filter file,$(origin PYTHON)),$(PYTHON_ENV_PYTHON),$(PYTHON))
CHECK_GRPCURL = $(if $(filter file,$(origin GRPCURL)),$(GRPCURL_TOOL),$(GRPCURL))
CHECK_TOOLS   = $(if $(filter file,$(origin PYTHON)),$(PYTHON_STAMP)) $(if $(filter file,$(origin GRPCURL)),$(GRPCURL_TOOL))

# Whatever docker the environment gives: the docker command on PATH (DOCKER
# names another) with its own DOCKER_HOST or context. Nothing here assumes
# where its socket is. A target that needs it checks that it answers first.
DOCKER ?= docker

# TEST_RUN names this run's containers: empty by default, so a run finds the
# containers an earlier one left. Two runs on one machine at once (make ci
# in two checkouts, say) each give their own, with ports of their own, since
# each run removes its containers when it ends:
#
#   make ci TEST_RUN=-b PG_PORT=55433 S3_SILO_PORT=59010 S3_RUSTFS_PORT=59011
TEST_RUN ?=
docker-answers = $(DOCKER) info > /dev/null 2>&1 || { echo "$@: docker does not answer: start it, or point DOCKER_HOST or the docker context at it" >&2; exit 2; }

# A throwaway Postgres, as CI's Postgres jobs have: the server's own image,
# on a port of its own on loopback so it never meets a Postgres the machine
# already runs. Its password is a fixed throwaway, as in CI: the container
# holds nothing and listens on 127.0.0.1 only.
PG_VERSION   ?= 18
PG_PORT      ?= 55432
PG_CONTAINER ?= nopacs-test-postgres$(TEST_RUN)
PG_PASSWORD  := throwaway-password
PG_URL        = postgres://postgres:$(PG_PASSWORD)@127.0.0.1:$(PG_PORT)/nopacs_test?sslmode=disable
# The same database as psql and pg_dump inside the container see it, where
# the server's port is the container's own.
PG_INNER_URL  = postgres://postgres:$(PG_PASSWORD)@127.0.0.1:5432/nopacs_test?sslmode=disable
pg-running    = [ -n "$$($(DOCKER) ps -q --filter 'name=^/$(PG_CONTAINER)$$' 2> /dev/null)" ]

.PHONY: pg-up pg-down test-postgres outside-postgres

# Ready means answering on TCP: during its first start the image runs a
# server on its Unix socket only, then restarts it, so pg_isready on the
# socket would answer too early.
pg-up:
	@$(docker-answers)
	@if $(pg-running); then \
		echo "pg-up: $(PG_CONTAINER) is already running"; \
	else \
		$(DOCKER) rm -f $(PG_CONTAINER) > /dev/null 2>&1 || true; \
		echo "pg-up: starting postgres:$(PG_VERSION) as $(PG_CONTAINER) on 127.0.0.1:$(PG_PORT)"; \
		$(DOCKER) run -d --name $(PG_CONTAINER) -p 127.0.0.1:$(PG_PORT):5432 \
			-e POSTGRES_PASSWORD=$(PG_PASSWORD) -e POSTGRES_DB=nopacs_test \
			postgres:$(PG_VERSION) > /dev/null; \
	fi
	@i=0; until $(DOCKER) exec $(PG_CONTAINER) pg_isready -q -h 127.0.0.1 -U postgres -d nopacs_test; do \
		i=$$((i + 1)); \
		if [ $$i -ge 60 ]; then $(DOCKER) logs $(PG_CONTAINER) 2>&1 | tail -20; echo "pg-up: Postgres did not answer within 60 s" >&2; exit 1; fi; \
		sleep 1; \
	done
	@echo "NOPACS_TEST_POSTGRES='$(PG_URL)'"

pg-down:
	@$(docker-answers)
	@$(DOCKER) rm -f $(PG_CONTAINER) > /dev/null 2>&1 || true
	@echo "pg-down: $(PG_CONTAINER) removed"

# The database drivers' tests and the contract suites against a real
# Postgres, as CI's Postgres job runs them: NOPACS_TEST_POSTGRES when it is
# set (CI's service container), or else the container of pg-up.
# NOPACS_REQUIRE_POSTGRES turns a missing database into a failure, so the
# Postgres tests cannot pass by skipping.
test-postgres:
	@set -e; url="$${NOPACS_TEST_POSTGRES:-}"; \
	if [ -z "$$url" ]; then \
		$(docker-answers); \
		$(pg-running) || { echo "test-postgres: no Postgres: run make pg-up, or set NOPACS_TEST_POSTGRES" >&2; exit 2; }; \
		url='$(PG_URL)'; \
	fi; \
	echo "NOPACS_REQUIRE_POSTGRES=1 $(GO) test -race -count=1 -v ./internal/driver/..."; \
	NOPACS_TEST_POSTGRES="$$url" NOPACS_REQUIRE_POSTGRES=1 $(GO) test -race -count=1 -v ./internal/driver/...; \
	echo "the reception end to end on Postgres: the pipeline, and serve with a study from storescu"; \
	NOPACS_TEST_POSTGRES="$$url" NOPACS_REQUIRE_POSTGRES=1 NOPACS_REQUIRE_DCMTK=1 NOPACS_DCMTK="$(DCMTK)" \
		$(GO) test -race -count=1 -v -run '^(TestStudyIsArchivedOnPostgres|TestAStudyIsArchivedThroughServe)$$' ./internal/receive ./internal/servecmd

# The two outside checks of Postgres against the same database. With pg-up's
# container, psql and pg_dump are the ones inside it, so their version is the
# server's (pg_dump refuses a newer server); with NOPACS_TEST_POSTGRES, the
# PSQL and PG_DUMP given.
outside-postgres:
	@set -e; \
	if [ -n "$${NOPACS_TEST_POSTGRES:-}" ]; then \
		$(MAKE) --no-print-directory check-master-postgres; \
		$(MAKE) --no-print-directory check-tenant-postgres; \
	else \
		$(docker-answers); \
		$(pg-running) || { echo "outside-postgres: no Postgres: run make pg-up, or set NOPACS_TEST_POSTGRES" >&2; exit 2; }; \
		export NOPACS_TEST_POSTGRES='$(PG_URL)' PSQL_URL='$(PG_INNER_URL)'; \
		$(MAKE) --no-print-directory check-master-postgres PSQL="$(DOCKER) exec -i $(PG_CONTAINER) psql" PG_DUMP="$(DOCKER) exec -i $(PG_CONTAINER) pg_dump"; \
		$(MAKE) --no-print-directory check-tenant-postgres PSQL="$(DOCKER) exec -i $(PG_CONTAINER) psql" PG_DUMP="$(DOCKER) exec -i $(PG_CONTAINER) pg_dump"; \
	fi

# The two S3 servers of CI's S3 job, at the images it pins by digest: Silo,
# the maintained fork of MinIO, and RustFS, a separate implementation, since
# one server's behavior is not the protocol's. S3_SERVERS picks some; each
# has a port of its own on loopback. Keep the digests in step with
# checks.yml's (it starts the servers with these targets).
S3_SERVERS     ?= silo rustfs
S3_SILO_PORT   ?= 59000
S3_RUSTFS_PORT ?= 59001
S3_KEY         := throwaway-key
S3_SECRET      := throwaway-secret
S3_SILO_IMAGE   := pgsty/silo@sha256:c20d112390b9a9ce8c11b8af95ad35bcaa62f4759663c09c936cd8d2b511f329
S3_RUSTFS_IMAGE := rustfs/rustfs@sha256:ee717072290bec9b11abfc0365f8310b00df1cd9b3639943edae47bdef05044a

# A shell function for the recipes below: the port of the server named by
# its argument, or a refusal of a name that is not one of the two.
s3-port = s3port() { case "$$1" in silo) echo $(S3_SILO_PORT) ;; rustfs) echo $(S3_RUSTFS_PORT) ;; *) echo "$@: no S3 server named $$1 (silo or rustfs)" >&2; return 2 ;; esac; }

.PHONY: s3-up s3-wait s3-down test-s3

s3-up:
	@$(docker-answers)
	@set -e; $(s3-port); for s in $(S3_SERVERS); do \
		port=$$(s3port $$s); name=nopacs-test-$$s$(TEST_RUN); \
		if [ -n "$$($(DOCKER) ps -q --filter "name=^/$$name\$$")" ]; then echo "s3-up: $$name is already running"; continue; fi; \
		$(DOCKER) rm -f $$name > /dev/null 2>&1 || true; \
		echo "s3-up: starting $$s as $$name on 127.0.0.1:$$port"; \
		case $$s in \
		silo) $(DOCKER) run -d --name $$name -p 127.0.0.1:$$port:9000 \
			-e MINIO_ROOT_USER=$(S3_KEY) -e MINIO_ROOT_PASSWORD=$(S3_SECRET) \
			$(S3_SILO_IMAGE) server /data > /dev/null ;; \
		rustfs) $(DOCKER) run -d --name $$name -p 127.0.0.1:$$port:9000 \
			-e RUSTFS_ACCESS_KEY=$(S3_KEY) -e RUSTFS_SECRET_KEY=$(S3_SECRET) \
			-e RUSTFS_ADDRESS=0.0.0.0:9000 \
			$(S3_RUSTFS_IMAGE) /data > /dev/null ;; \
		esac; \
	done
	@$(MAKE) --no-print-directory s3-wait

# Either server answers its port before it can serve; the tests create their
# bucket, so a listing that is answered at all is enough.
s3-wait:
	@set -e; $(s3-port); for s in $(S3_SERVERS); do \
		port=$$(s3port $$s); i=0; \
		until curl -sS -o /dev/null http://127.0.0.1:$$port/ 2> /dev/null; do \
			i=$$((i + 1)); \
			if [ $$i -ge 60 ]; then $(DOCKER) logs nopacs-test-$$s$(TEST_RUN) 2>&1 | tail -20; echo "s3-wait: $$s did not answer within 60 s" >&2; exit 1; fi; \
			sleep 1; \
		done; \
		echo "NOPACS_TEST_S3='http://$(S3_KEY):$(S3_SECRET)@127.0.0.1:'$$port'/nopacs-test'   # $$s"; \
	done

s3-down:
	@$(docker-answers)
	@for s in $(S3_SERVERS); do $(DOCKER) rm -f nopacs-test-$$s$(TEST_RUN) > /dev/null 2>&1 || true; echo "s3-down: nopacs-test-$$s$(TEST_RUN) removed"; done

# The S3 driver's tests against each server s3-up started.
# NOPACS_REQUIRE_S3 turns a server that is not there into a failure, so a
# pass means the suite ran.
test-s3:
	@$(docker-answers)
	@set -e; $(s3-port); for s in $(S3_SERVERS); do \
		port=$$(s3port $$s); \
		[ -n "$$($(DOCKER) ps -q --filter "name=^/nopacs-test-$$s$(TEST_RUN)\$$")" ] || { echo "test-s3: $$s is not running; run make s3-up" >&2; exit 2; }; \
		echo "test-s3: $$s on 127.0.0.1:$$port: $(GO) test -race -count=1 -v ./internal/blob/s3/..."; \
		NOPACS_TEST_S3="http://$(S3_KEY):$(S3_SECRET)@127.0.0.1:$$port/nopacs-test" NOPACS_REQUIRE_S3=1 \
			$(GO) test -race -count=1 -v ./internal/blob/s3/...; \
		echo "test-s3: the reception end to end on $$s: the pipeline, and serve with a study from storescu"; \
		NOPACS_TEST_S3="http://$(S3_KEY):$(S3_SECRET)@127.0.0.1:$$port/nopacs-test" NOPACS_REQUIRE_S3=1 \
			NOPACS_REQUIRE_DCMTK=1 NOPACS_DCMTK="$(DCMTK)" \
			$(GO) test -race -count=1 -v -run '^(TestStudyIsArchivedOnS3|TestAStudyIsArchivedThroughServe)$$' ./internal/receive ./internal/servecmd; \
	done

# Each binary was built with CGO_ENABLED=0; this shows the Linux one is in
# fact static, so a change that brings in C cannot pass unseen. grep without
# -q: it reads all its input, so the command before it is never cut off by a
# closed pipe. The binary is run only where it can be.
.PHONY: check-static
check-static:
	@set -e; for prog in nopacs nopacs-server; do \
		test -f $(BIN)/$$prog-linux-amd64 || { echo "check-static: no $(BIN)/$$prog-linux-amd64; run make build first" >&2; exit 1; }; \
		echo "file $(BIN)/$$prog-linux-amd64"; file $(BIN)/$$prog-linux-amd64 | grep 'statically linked'; \
		$(GO) version -m $(BIN)/$$prog-linux-amd64 | grep -x "$$(printf '\tbuild\tCGO_ENABLED=0')"; \
		if [ "$$(uname -s)/$$(uname -m)" = Linux/x86_64 ]; then \
			echo "./$(BIN)/$$prog-linux-amd64 version"; ./$(BIN)/$$prog-linux-amd64 version; \
		else \
			echo "check-static: $(BIN)/$$prog-linux-amd64 is not run on $$(uname -s)/$$(uname -m)"; \
		fi; \
	done

# Every outside check that needs a tool that is not Go: Python, sqlite3,
# bash, grpcurl, curl, a pseudo-terminal, and psql and pg_dump when a Postgres
# is up (NOPACS_TEST_POSTGRES, or pg-up's container). OUTSIDE_POSTGRES=no
# leaves the two Postgres checks out, for a caller that runs them itself.
OUTSIDE_POSTGRES ?= auto

.PHONY: outside
outside: $(CHECK_TOOLS)
	@set -e; m() { echo; echo "==> make $$*"; $(MAKE) --no-print-directory "$$@"; }; \
	m check-seal PYTHON="$(CHECK_PYTHON)"; \
	m check-master-sqlite; \
	m check-tenant-sqlite; \
	m check-json PYTHON="$(CHECK_PYTHON)"; \
	m check-completion; \
	m check-tui PYTHON="$(CHECK_PYTHON)"; \
	m check-server PYTHON="$(CHECK_PYTHON)" GRPCURL="$(CHECK_GRPCURL)"; \
	if [ "$(OUTSIDE_POSTGRES)" = no ]; then \
		echo "outside: check-master-postgres and check-tenant-postgres left out (OUTSIDE_POSTGRES=no)"; \
	elif [ -n "$${NOPACS_TEST_POSTGRES:-}" ] || { $(DOCKER) info > /dev/null 2>&1 && $(pg-running); }; then \
		m outside-postgres; \
	else \
		echo "outside: skipped check-master-postgres and check-tenant-postgres: no Postgres (make pg-up, or set NOPACS_TEST_POSTGRES)"; \
	fi

# make ci: every job of checks.yml, one after another, cheapest first so a
# mistake fails in seconds, not after the tests: the comments, formatting,
# vet, lint and the contract; then the tests, the builds, the outside checks,
# the server's checks, reception's speed against origin/develop, Postgres at each version CI tests, both S3 servers,
# the proof as CI's proof job runs it (strict on Linux, where every check can
# run), and the proof of reception last when dcmtk is found. Docker is checked first, since half of it needs docker.
#
# What CI runs that cannot run here is said, and listed again at the end:
# the 32-bit tests need a Linux x86-64 machine (elsewhere their binaries are
# built and vetted, not run); the local socket refused to a second user needs
# that user (NOPACS_TEST_OTHER_USER names one, as CI makes one with sudo);
# the Windows and macOS jobs need those systems (lint and vet run for
# Windows here, and the tests run on whichever system this is); and the
# Linux job's second Go release is CI's alone.
#
# It tests the containers it starts, not a database or server the
# environment names. Every container is removed when the run ends, by
# success, failure or an interrupt (the trap), including the Postgres the
# proof stops and starts; a pg-up container of the same name is replaced.
CI_PG_VERSIONS ?= 16 18

.PHONY: ci
ci:
	@set -e; \
	start=$$(date +%s); os=$$(uname -s); arch=$$(uname -m); skipped=""; \
	skip() { skipped="$$skipped|$$*"; echo "ci: not run here: $$*"; }; \
	step() { echo; echo "==> make $$*"; $(MAKE) --no-print-directory "$$@"; }; \
	$(DOCKER) info > /dev/null 2>&1 || { echo "ci: docker does not answer: Postgres, S3 and the proof need it; start it, or point DOCKER_HOST or the docker context at it" >&2; exit 2; }; \
	trap '$(MAKE) --no-print-directory pg-down s3-down > /dev/null 2>&1 || true' EXIT; \
	trap 'exit 130' INT TERM; \
	unset NOPACS_TEST_POSTGRES PSQL_URL NOPACS_TEST_S3; \
	echo "ci: $$($(GO) version) on $$os/$$arch"; \
	step check-comments; \
	step fmt; \
	step vet; \
	step lint; \
	echo; echo "==> GOOS=windows $(GO) vet ./..."; \
	GOOS=windows $(GO) vet ./...; \
	step proto-lint; \
	step proto-breaking; \
	step proto-check; \
	step tools-python; \
	py="$(CHECK_PYTHON)"; \
	step proto-python PYTHON="$$py"; \
	step vuln; \
	step test; \
	step test-scale; \
	if [ "$$os/$$arch" = Linux/x86_64 ]; then \
		step test-386; \
	else \
		echo; echo "==> the linux/386 test binaries, built and not run"; \
		GOOS=linux GOARCH=386 CGO_ENABLED=0 $(GO) test -count=1 -exec true ./... > /dev/null; \
		skip "make test-386: 32-bit Linux binaries do not run on $$os/$$arch (built and vetted instead)"; \
	fi; \
	step fuzz FUZZTIME=10s; \
	step build; \
	step size; \
	step check-static; \
	step outside PYTHON="$$py" OUTSIDE_POSTGRES=no; \
	if [ -n "$${NOPACS_TEST_OTHER_USER:-}" ]; then \
		echo; echo "==> the local socket refused to $$NOPACS_TEST_OTHER_USER"; \
		$(GO) test -count=1 -v -run '^TestSocketRefusesOtherUser$$' ./internal/server; \
	else \
		skip "TestSocketRefusesOtherUser: needs a second operating system user, named by NOPACS_TEST_OTHER_USER (CI makes one with sudo)"; \
	fi; \
	step check-e2e; \
	step bench-gate; \
	for v in $(CI_PG_VERSIONS); do \
		step pg-down; \
		step pg-up PG_VERSION=$$v; \
		step test-postgres; \
		if [ "$$os/$$arch" = Linux/x86_64 ] && [ "$$v" = 18 ]; then \
			echo; echo "==> the Postgres driver as linux/386"; \
			NOPACS_TEST_POSTGRES='$(PG_URL)' NOPACS_REQUIRE_POSTGRES=1 GOOS=linux GOARCH=386 CGO_ENABLED=0 $(GO) test -count=1 ./internal/driver/postgres; \
		elif [ "$$v" = 18 ]; then \
			skip "the Postgres driver as linux/386: 32-bit Linux binaries do not run on $$os/$$arch"; \
		fi; \
		echo; echo "==> query plans at millions of rows, Postgres $$v"; \
		NOPACS_TEST_SCALE=1 NOPACS_TEST_POSTGRES='$(PG_URL)' NOPACS_REQUIRE_POSTGRES=1 $(GO) test -count=1 -v -run '^TestScalePipelinePlans$$' ./internal/driver/postgres; \
		step outside-postgres; \
	done; \
	step pg-down; \
	step s3-up; \
	step test-s3; \
	step s3-down; \
	step pg-up PG_VERSION=18; \
	strict=0; [ "$$os" = Linux ] && strict=1; \
	[ "$$strict" = 1 ] || skip "NOPACS_PROVE_STRICT=1: on $$os a check of the proof may be skipped (the second user needs sudo on Linux); the proof says which"; \
	echo; echo "==> make prove (NOPACS_PROVE_STRICT=$$strict)"; \
	NOPACS_PROVE_PG_CONTAINER=$(PG_CONTAINER) NOPACS_PROVE_PG_PORT=$(PG_PORT) NOPACS_PROVE_STRICT=$$strict \
		$(MAKE) --no-print-directory prove PYTHON="$$py" GRPCURL="$(CHECK_GRPCURL)" DCMTK="$(DCMTK)"; \
	dcmtk="$(DCMTK)"; storescu="$${dcmtk:+$$dcmtk/}storescu"; \
	if command -v "$$storescu" > /dev/null && "$$storescu" --version 2> /dev/null | grep -q "^\$$dcmtk:"; then \
		step s3-up S3_SERVERS=silo; \
		step prove-reception PYTHON="$$py" DCMTK="$$dcmtk" PROVE_S3="http://$(S3_KEY):$(S3_SECRET)@127.0.0.1:$(S3_SILO_PORT)/prove-reception" \
			PROVE_POSTGRES="postgres://postgres:$(PG_PASSWORD)@127.0.0.1:$(PG_PORT)/postgres?sslmode=disable" \
			PROVE_PSQL="$(DOCKER) exec -i $(PG_CONTAINER) psql -U postgres"; \
		step s3-down S3_SERVERS=silo; \
	else \
		skip "make prove-reception: no dcmtk storescu (DCMTK=dir names its directory; pynetdicom installs a program of the same name)"; \
	fi; \
	skip "the Windows and macOS jobs and Go releases other than this one run in CI only (lint and vet for Windows ran here)"; \
	echo; echo "ci: passed in $$(( $$(date +%s) - start )) s on $$os/$$arch. Not run here:"; \
	printf '%s\n' "$$skipped" | tr '|' '\n' | sed -n 's/^./  - &/p'
