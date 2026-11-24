# Reading the archive

[Documentation index](../README.md) · [Architecture](../develop/architecture.md) · [A reception, end to end](receiving.md) · [Receiving through the API](../api/reception-walkthrough.md)

Everything archived is read back through the API, with a key that holds a grant
on the tenant, and every read is audited. The contract is in
`api/proto/nopacs/v1`; the handlers are [internal/api](../../internal/api/doc.go).

| What you want | Where it is served |
|---|---|
| Subjects, sessions and scans, with filters and pages | `CatalogService` (`nopacs catalog`) |
| Each instance's DICOM header as JSON | `MetadataService` (`nopacs scan show --metadata`) |
| The files of a scan, checked as they stream | `DataService` over gRPC and Connect; one instance over HTTP, resumable by range (`nopacs download`) |
| Search, retrieve, metadata, frames and bulk data in the DICOMweb way | QIDO-RS and WADO-RS under `/tenants/<name>/dicom-web` (PS3.18 10.4, 10.6) |
| Store through HTTP | STOW-RS at the same path, into a receiver that lists the key (PS3.18 10.5); see [a reception](receiving.md) |

## How a read goes

After the [path every request takes](../develop/architecture.md#the-path-of-a-request), a
read is answered from the catalog in the tenant's database, from what each scan's
[sidecar](../../internal/receive/sidecar/doc.go) holds on the storage (headers and
[frame tables](../../internal/receive/frames/doc.go)), and from the instance files
themselves, which [checkedread](../../internal/checkedread/doc.go) compares with the
manifest as they go out.

```mermaid
flowchart LR
    client["Caller with a key"] --> check["front step and access check<br/>role on the tenant, audit entry"]
    check --> catalogsvc["CatalogService<br/>subjects, sessions, scans"]
    check --> metasvc["MetadataService<br/>instance headers as JSON"]
    check --> datasvc["DataService and download route<br/>files, resumable by range"]
    check --> web["DICOMweb routes<br/>QIDO-RS, WADO-RS"]
    catalogsvc --> db[("tenant database<br/>catalog")]
    web --> db
    metasvc --> sidecar[("storage: scan sidecar<br/>headers, frame tables")]
    web --> sidecar
    metasvc -->|"what the sidecar lacks"| files
    web --> checked["checkedread<br/>compares length and SHA-256<br/>as bytes go out"]
    datasvc --> checked
    checked --> files[("storage: instance files<br/>and manifest")]
```

[dicomweb](../../internal/dicomweb/doc.go) holds the protocol without the archive: what
a path names, how a search is read, matching, paging, media types and Warning
headers. [place](../../internal/place/doc.go) opens the storage a scan lives on.

## What a read checks, and its bounds

Each read is checked as its bytes go out and is bounded: what a check cuts, what it
does not check, the 413 limits of a search and the open-read slots are in
[internal/api](../../internal/api/doc.go) (its bounds of the reads) and
[internal/checkedread](../../internal/checkedread/doc.go) (what a checked read guarantees).
