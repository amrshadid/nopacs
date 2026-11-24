# DICOMweb

[Documentation index](../README.md) · [The API: an overview](overview.md) · [Reading the archive](../use/reading.md) · [A reception, end to end](../use/receiving.md)

Each tenant serves a DICOMweb (PS3.18) service under
`https://host:port/tenants/<tenant>/dicom-web`, for viewers and tools that speak it.
It uses the same keys, roles and audit as the rest of the API: a key needs the **reader** role on the
tenant to search and retrieve, and a **STOW-RS** receiver that lists the key (and the
key must hold writer or more) to store.

| What | Request | Standard |
|---|---|---|
| Search studies, series and instances | `GET .../studies`, `.../series`, `.../instances` and the nested forms | QIDO-RS, PS3.18 10.6 |
| Retrieve instances, a study or a series | `GET .../studies/<uid>[/series/<uid>[/instances/<uid>]]` | WADO-RS, PS3.18 10.4 |
| Retrieve metadata | `.../metadata` | WADO-RS |
| Retrieve frames and bulk data | `.../frames/<list>`, bulk data URIs | WADO-RS |
| Store | `POST .../studies` into a STOW-RS receiver | STOW-RS, PS3.18 10.5 |

Search answers come from the catalog, and the metadata from files kept beside each
scan; retrieved instances are sent as they are stored and compared with their SHA-256
as they go out.

## What it does not do

- **No transcoding.** An instance or a frame is answered as it is stored, or refused
  with `406` when the request accepts only another transfer syntax.
- **No fuzzy matching of person names.** A request that asks for it is matched
  literally, and a `Warning` header says so.
- **No matching inside a sequence.** A key with a dotted path is refused with `400`.
- **No rendered images or thumbnails**: those resources answer `404`.
- A tenant that de-identifies answers a stored instance by the SOP Instance UID it
  was sent with and gives no Retrieve URL: the archive holds it under remapped UIDs,
  and a URL by them would hand the sender the mapping.

A search that would be too large is refused (`413`); reads past the open-read bounds
are refused with `429` and `Retry-After` (`read.per-caller`, `read.per-server`).

## Using a viewer

A viewer is a browser page, so it needs the server to list its origin
(`api.cors-origins`) and a key it can send. A viewer should be given a short-lived
**reader** key made for it, not an operator's. A key that works only with a TLS client
certificate is possible too. A recipe for a viewer behind a login is not written yet.

## Checked against

QIDO-RS and WADO-RS are checked against pydicom (`make check-dicomweb`), and STOW-RS
against `dicomweb-client` (`make check-stow`); see
[gates and outside checks](../develop/gates.md).
