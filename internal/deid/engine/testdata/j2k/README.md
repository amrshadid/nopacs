# JPEG 2000 test data

Synthetic images only: gradients, noise and flat fields drawn by programs, no
person's image. Each is small (under 6 KiB).

| Files | Written by | What they cover |
|---|---|---|
| `opj_*.j2k`, `g97_*.j2k`, `n97_*.j2k`, `gradient.j2k`, `noise.j2k`, `sparse.j2k` | OpenJPEG's `opj_compress`, copied from go-dicom's `compress/jpeg2000/testdata` | tiles, tile-parts, precincts, progression orders and changes (POC), region of interest (RGN), every code-block style, subsampled components, reversible and irreversible transforms; each with OpenJPEG's comment segment |
| `ht_*.j2k` | an HTJ2K encoder, copied from go-dicom's `compress/jpeg2000/testdata` | High-Throughput code-blocks (CAP), cleanup, significance propagation and refinement passes, tiles, RPCL, beside Part 1 code-blocks |
| `htj2k_*.dcm` | copied from go-dicom's `dataset/testdata/htj2k` | DICOM files in the three HTJ2K transfer syntaxes: lossless, lossless RPCL (16 bits), irreversible |
| `opj_tiles_tlm_plt_comment.j2k` | `opj_compress -t 16,16 -TP R -PLT -TLM -SOP -EPH -n 3 -C "made for a test"` over a 32 by 32 noise image | four tiles of three tile-parts each, with TLM in the main header, PLT in every tile-part header, SOP and EPH markers and a comment segment |
