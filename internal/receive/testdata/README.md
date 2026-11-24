# Test files

Every DICOM file here is a public test file of pydicom, copied unchanged
from `src/pydicom/data/test_files` of the pydicom repository
(github.com/pydicom/pydicom). pydicom is released under the MIT license,
copyright 2008-2020 Darcy Mason and pydicom contributors; its license
covers these files, and the notice is kept with them here. None holds data
of a real patient.

| File here | pydicom's name | What it is |
|---|---|---|
| `ct2n-6293.dcm`, `ct2n-6924.dcm` | `dicomdirtests/98892001/CT2N/6293`, `6924` | CT, one study, series 2 |
| `ct5n-2062.dcm` ... `ct5n-3353.dcm` | `dicomdirtests/98892001/CT5N/2062` ... `3353` | CT, the same study, series 5 |
| `MR_small.dcm` | `MR_small.dcm` | MR, explicit VR little endian |
| `MR_small_RLE.dcm` | `MR_small_RLE.dcm` | the same instance, RLE compressed: same SOP Instance UID, other bytes |
| `MR_small_jp2klossless.dcm` | `MR_small_jp2klossless.dcm` | the same instance, JPEG 2000 lossless |
| `MR_small_jpeg_ls_lossless.dcm` | `MR_small_jpeg_ls_lossless.dcm` | the same instance, JPEG-LS lossless |
| `MR_truncated.dcm` | `MR_truncated.dcm` | MR_small cut short |
| `liver_1frame.dcm` | `liver_1frame.dcm` | a segmentation, an enhanced multi-frame image with functional groups |
| `SC_rgb_rle_2frame.dcm` | `SC_rgb_rle_2frame.dcm` | RLE, two frames of one fragment each, with a Basic Offset Table |
| `jpeg_3frame_nobot.dcm` | made from `examples_ybr_color.dcm` | JPEG baseline, its first three frames in three fragments each with an empty Basic Offset Table, so only the end-of-image markers divide the frames |

`jpeg_3frame_nobot.dcm` is not in pydicom's corpus: it is made from one of
its files, with its own SOP Instance UID, by
`python3 scripts/check/frames-pydicom.py build <pydicom's test_files> <dir>`
(`pydicom.encaps.encapsulate` with three fragments a frame and no Basic
Offset Table), which makes the same bytes on every run.

## pydicom's license

```
Copyright (c) 2008-2020 Darcy Mason and pydicom contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```
