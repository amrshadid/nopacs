# Pixel templates: filling burned-in text by device

[Documentation index](../README.md) · [De-identification](../concepts/deidentification.md) · [Profiles](profiles.md) · [The command line](command-line.md)

Some scanners draw text into the image itself: a name, an identifier, a date. It
is in the pixels, so no rule about the header removes it. When a device draws it
in the same place on every image of one size, a profile can fill that place
without a person drawing the rectangles into every profile: a **pixel
template** says, for one device and one image size, which rectangles to fill.

A template is for a device and a size:

| Match | How it is compared |
|---|---|
| Manufacturer (0008,0070) | with case folded, and the padding of a DICOM value (spaces, NULs) removed |
| Manufacturer's Model Name (0008,1090) | the same |
| Rows (0028,0010) and Columns (0028,0011) | exactly |
| SOP Class UID (0008,0016) | exactly; empty means any class |

Where a template for the file's SOP class and one for any class both fit, the
one for the class is used. The values are read from the file **as it was
received**, before the profile changes anything, so a profile that removes the
manufacturer still finds its template.

**The server ships no template.** Which rectangles hold text is for a site to
know of its own devices, and a template list taken from another project is not
part of NoPACs (a template is only as good as its checking: see
[what is weak](#what-is-weak)). Start from your own devices, add a template, send
a test image of that device, and look at it.

## The profile's side

A pixel rule with the action `redact_by_template` fills the template's
rectangles in every frame, exactly as `redact` fills its own (the image is then
written uncompressed, Burned In Annotation is written `NO`, and the Clean Pixel
Data code is recorded):

```json
{
  "bases": ["annex-e/basic:2026d"],
  "pixelRules": [{"name": "by-device", "redactByTemplate": {}}]
}
```

The rule takes the same optional `when` conditions as any pixel rule, and, as for
any, the last pixel rule that matches an image decides. A profile that uses it is
of language edition 4, so a server that does not know the action refuses to run
the version rather than archive an image a template should have filled.

An image whose pixels may carry burned-in text (the same test as for the gate in
[de-identification](../concepts/deidentification.md)) that the rule matches and
for which **no template fits** is held in quarantine as `deid_failed`, cause
`no_pixel_template`. It is never archived unfilled. Add the template and release
the file (`nopacs quarantine release`): it is de-identified again and filled. An
image that is not at risk, such as a CT that does not say it carries text, is
left as it is when no template fits.

## Loading templates

All changes need the admin role on the tenant and are refused to a masked key;
each is audited with the number of templates (or the key of the one removed),
never an image's value. Listing needs operator.

One template from flags:

```sh
nopacs deid template add lab1 --manufacturer "ACME Imaging" --model "Scan One" \
    --rows 480 --columns 640 \
    --rect 0,0,640,40 --rect 10,10,120,20,from-right,from-bottom
```

A `--rect` is `x,y,width,height` in pixels from the top left corner. After the
four numbers, `from-right` measures x from the right edge and `from-bottom`
measures y from the bottom, so a stamp in a corner stays in it. A template holds 1
to 64 rectangles, each inside the image.

A list from a file, JSON or CSV, at most 1000 templates at a time, all or none:

```sh
nopacs deid template import lab1 templates.json
nopacs deid template import lab1 templates.csv
```

JSON is a list of templates, or an object with the list as `templates`. Fields
the format does not name are refused:

```json
[
  {
    "manufacturer": "ACME Imaging",
    "model": "Scan One",
    "rows": 480,
    "columns": 640,
    "sop_class": "",
    "rectangles": [
      {"x": 0, "y": 0, "width": 640, "height": 40},
      {"x": 10, "y": 10, "width": 120, "height": 20, "from_right": true, "from_bottom": true}
    ]
  }
]
```

CSV has a header line and a line for each rectangle. Lines that follow one another
with the same manufacturer, model, rows, columns and sop_class are one template.
`from_right` and `from_bottom` (true or false) are optional columns:

```csv
manufacturer,model,rows,columns,sop_class,x,y,width,height,from_right,from_bottom
ACME Imaging,Scan One,480,640,,0,0,640,40,,
ACME Imaging,Scan One,480,640,,10,10,120,20,true,true
ACME Imaging,Scan Two,512,512,1.2.840.10008.5.1.4.1.1.6.1,0,0,512,30,,
```

The command refuses a file with a ragged line, a field that is not a number where
one belongs, or a header that is not the columns', naming the line and never
quoting it, before anything is sent. The server refuses the whole request, storing
nothing, for a rectangle with a negative position or a side outside 1 to 65535 or
reaching past the image, a SOP class that is not a UID, two templates of one
device, size and SOP class (even when their text differs only in case), and a
template the tenant already has (remove it first). A tenant holds at most 10000
templates.

```sh
nopacs deid template list lab1
nopacs deid template remove lab1 --manufacturer "ACME Imaging" --model "Scan One" \
    --rows 480 --columns 640
```

The same calls are in the API: `AddPixelTemplates`, `RemovePixelTemplate` and
`ListPixelTemplates` of `DeidService`.

## Checking a template

A template is a claim about where a device draws text, so test it before the
data comes:

1. Add it, and send an image of that device and size through a receiver whose
   profile has the `redact_by_template` rule (a test tenant, a synthetic or
   public image).
2. Read the archived file with a second tool (pydicom, dcmtk's `dcm2pnm`) and
   look at it: the text is gone, nothing else is.
3. Send an image of the same device in another size: it is held with
   `no_pixel_template`, because a template is exact on size.

## What is weak

- A template is exact on rows and columns. A device that writes the text at
  another position when the image is cropped or zoomed needs a template for each
  size.
- A device whose text moves from image to image (a cursor, a measurement) is not
  covered by a fixed rectangle; use the detector (`detect`) for it, which can miss.
- A template fills what it names. If the text is outside the rectangles, it stays;
  the server cannot tell.
- Trying a profile version over earlier receptions (`profile try`, a change
  trial) and re-running one over the archive judge a template rule without the
  tenant's templates, so they count an image at risk as held for want of a
  template.
