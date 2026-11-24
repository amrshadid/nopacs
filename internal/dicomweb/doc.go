// Package dicomweb is the protocol of DICOMweb (PS3.18) as NoPACs serves it
// per tenant, without the archive behind it: the resources a request path
// names (PS3.18 10.4, 10.6), the query of a search read into matching keys,
// the attributes to return and the page asked for (PS3.18 8.3.4, 10.6.1),
// the matching of a value against a key (PS3.4 C.2.2.2), the result of a
// search written as DICOM JSON by go-dicom (PS3.18 F.2), and the media
// types and transfer syntaxes a request accepts (PS3.18 8.7).
//
// It reads nothing and writes no answer: internal/api finds what a
// resource names in the catalog and the storages, and writes the answer
// with what this package decides. So every rule of the protocol is here,
// tested without an archive, and every rule of the archive (where a file
// is, whether a key may read it) is there.
//
// What it does not do, and why:
//
//   - No transcoding. An instance or a frame is answered as it is stored,
//     or refused with 406 when the request accepts only another transfer
//     syntax: the stored bytes are the only answer certain to be the
//     instance archived, and decoding is a later capability.
//   - No fuzzy matching of person names: a request that asks for it is
//     matched literally and told so in a Warning header (PS3.18 10.6.1.2.2).
//   - No matching inside a sequence: a key with a dotted path is refused
//     with 400, since the catalog and the metadata files hold no index of
//     a sequence's items.
package dicomweb
