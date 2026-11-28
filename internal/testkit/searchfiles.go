package testkit

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/query"
)

// SearchFile is one synthetic file of a search tenant: a public test file
// whose header values were replaced.
type SearchFile struct {
	// Bytes is the whole Part 10 file.
	Bytes []byte
	// Study, Series and SOP are its three UIDs.
	Study, Series, SOP string
}

// searchBases are the public files the search tenant's files are made from:
// explicit VR little endian images with pixel data, so a file the library
// writes again is one reception takes.
var searchBases = []string{"MR_small.dcm", "ct2n-6293.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm"}

// searchAttributes are the keywords of every attribute a search table holds
// (internal/search), and the element each file loses before it gets its own
// values, so an attribute a scan was not given is absent from its files.
var searchAttributes = []string{
	"Modality", "SeriesDescription", "SeriesNumber", "ProtocolName", "StudyDescription", "BodyPartExamined", "Manufacturer",
	"ManufacturerModelName", "StationName", "SequenceName", "PatientSex", "PatientAge", "MagneticFieldStrength", "SliceThickness",
	"RepetitionTime", "EchoTime", "FlipAngle", "AcquisitionDate", "AcquisitionDateTime", "ImageType", "ScanningSequence", "OperatorsName",
	"PatientID", "StudyInstanceUID", "SeriesInstanceUID", "SOPInstanceUID", "StudyDate", "SpecificCharacterSet",
}

// The pools a scan's values are drawn from. Each holds the shapes a
// comparison must tell apart: case, accents, an eszett, an underscore and a
// percent sign, a value another is part of, several values, and values of
// one attribute that are numbers and dates written in the several ways
// DICOM allows.
var (
	poolModality    = []string{"MR", "MR", "MR", "CT", "CT", "PT", "US"}
	poolDescription = []string{"T1_MPRAGE", "t1 mprage repeat", "T2 FLAIR", "Localizer", "DWI b1000", "Müller Straße", "Ünï Täst 100%",
		"CT Head 5mm", "fMRI rest run 1", "fMRI rest run 2", "T1XMPRAGE", "Chest 1.25mm"}
	poolProtocol = []string{"T1W sag", "T1W axial", "loc", "head", "fmri", "T2W cor"}
	poolStudy    = []string{"Brain research", "BRAIN follow-up", "Chest survey", "Whole body", "Épreuve cérébrale"}
	poolBody     = []string{"HEAD", "BRAIN", "CHEST", "ABDOMEN"}
	poolMaker    = []string{"SIEMENS", "siemens", "GE MEDICAL SYSTEMS", "Philips"}
	poolModel    = []string{"Prisma", "Skyra", "Signa", "Ingenia"}
	poolStation  = []string{"MRC1", "MRC2", "CT01"}
	poolSequence = []string{"*tfl3d1_16ns", "*se2d1", "*epfid2d1_64", "FSE"}
	poolField    = []string{"1.5", "3", "7"}
	poolThick    = []string{"0.9", "1", "1.2", "2.5", "5"}
	poolTR       = []string{"2300", "500", "8.2", "4500"}
	poolTE       = []string{"2.98", "30", "100", "12"}
	poolFlip     = []string{"8", "90", "15"}
	poolImage    = []string{`ORIGINAL\PRIMARY\M\NORM`, `DERIVED\SECONDARY`, `ORIGINAL\PRIMARY\LOCALIZER`, `ORIGINAL\PRIMARY\AXIAL`}
	poolScanSeq  = []string{"GR", "SE", `SE\IR`, "EP"}
	poolOperator = []string{"Doe^John", "Roe^Ann", `Doe^John\Roe^Ann`}
	poolSex      = []string{"M", "F", "O"}
)

// SearchFiles returns the files of a small tenant a search is checked
// against: five subjects of one to three studies, each study of two to five
// scans, each scan of one or two files, every file a public test file read
// by go-dicom, given values drawn from seed by the pools above, and written
// again in its own transfer syntax. A scan has an attribute or not, one in
// five of a scan's attributes is left out; the second file of a scan has a
// different Slice Thickness and Manufacturer and the lower SOP Instance UID,
// so the value a scan's row takes from its lowest file is not the first
// file's. corpusDir holds the public files (internal/receive/testdata).
func SearchFiles(tb testing.TB, corpusDir string, seed uint64) []SearchFile {
	tb.Helper()
	r := rand.New(rand.NewPCG(seed, 11)) // #nosec G404 -- a seeded generator is the point: one seed, one tenant.
	pick := func(pool []string) string { return pool[r.IntN(len(pool))] }
	var out []SearchFile
	scanNo := 0
	for subject := range 5 {
		patient := fmt.Sprintf("SRCH-%02d", subject+1)
		sex, age := pick(poolSex), fmt.Sprintf("%03dY", 20+r.IntN(60))
		for study := range 1 + r.IntN(3) {
			studyUID := fmt.Sprintf("1.2.826.0.1.3680043.8.498.%d.%d.%d", seed, subject+1, study+1)
			day := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, r.IntN(1400))
			studyDescription := pick(poolStudy)
			for s := range 2 + r.IntN(4) {
				scanNo++
				seriesUID := fmt.Sprintf("1.2.826.0.1.3680043.8.498.%d.%d.%d.%d", seed, subject+1, study+1, s+1)
				values := map[string]string{
					"PatientID": patient, "PatientSex": sex, "PatientAge": age, "StudyInstanceUID": studyUID, "SeriesInstanceUID": seriesUID,
					"StudyDate": day.Format("20060102"), "StudyDescription": studyDescription, "SeriesNumber": strconv.Itoa(s + 1),
					"Modality": pick(poolModality),
				}
				// An attribute some scans lack: each is left out one time in five.
				for _, a := range []struct {
					key  string
					pool []string
				}{{"SeriesDescription", poolDescription}, {"ProtocolName", poolProtocol}, {"BodyPartExamined", poolBody}, {"Manufacturer", poolMaker},
					{"ManufacturerModelName", poolModel}, {"StationName", poolStation}, {"SequenceName", poolSequence},
					{"MagneticFieldStrength", poolField}, {"SliceThickness", poolThick}, {"RepetitionTime", poolTR}, {"EchoTime", poolTE},
					{"FlipAngle", poolFlip}, {"ImageType", poolImage}, {"ScanningSequence", poolScanSeq}} {
					if v := pick(a.pool); r.IntN(5) != 0 {
						values[a.key] = v
					}
				}
				if r.IntN(3) == 0 {
					values["OperatorsName"] = pick(poolOperator)
				}
				acquired := day.AddDate(0, 0, r.IntN(2))
				if r.IntN(10) != 0 {
					values["AcquisitionDate"] = acquired.Format("20060102")
				}
				switch r.IntN(6) {
				case 0, 1, 2:
					values["AcquisitionDateTime"] = acquired.Format("20060102") + fmt.Sprintf("%02d%02d%02d", r.IntN(24), r.IntN(60), r.IntN(60))
				case 3:
					values["AcquisitionDateTime"] = acquired.Format("20060102150405") + ".250000"
				case 4:
					values["AcquisitionDateTime"] = acquired.Format("200601")
				}
				instances := 1 + r.IntN(2)
				for i := range instances {
					v := make(map[string]string, len(values)+1)
					for k, x := range values {
						v[k] = x
					}
					// Instance 0 is written first and has the higher UID.
					v["SOPInstanceUID"] = fmt.Sprintf("%s.%d", seriesUID, 9-i)
					if i > 0 {
						v["SliceThickness"], v["Manufacturer"] = "3.3", "OTHER MAKER"
					}
					base := searchBases[scanNo%len(searchBases)]
					out = append(out, SearchFile{Bytes: writeSearchFile(tb, filepath.Join(corpusDir, base), v), Study: studyUID, Series: seriesUID, SOP: v["SOPInstanceUID"]})
				}
			}
		}
	}
	return out
}

// writeSearchFile reads the public file at path with go-dicom, replaces
// every element a search holds by the value in values (or leaves it out),
// and writes it again in the transfer syntax it had. The text is UTF-8 and
// says so (PS3.5 6.1.2.5.3), since a value holds accents.
func writeSearchFile(tb testing.TB, path string, values map[string]string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- a test's own corpus file.
	if err != nil {
		tb.Fatal(err)
	}
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(raw)), filereader.ReadOptions{})
	if err != nil {
		tb.Fatalf("%s: %v", path, err)
	}
	ds := df.GetDataset()
	values["SpecificCharacterSet"] = "ISO_IR 192"
	for _, k := range searchAttributes {
		f, err := query.ParseField(k)
		if err != nil {
			tb.Fatal(err)
		}
		t := tag.Tag(f.Path[0].Tag)
		ds.Remove(t)
		v, ok := values[k]
		if !ok {
			continue
		}
		b := []byte(v)
		if len(b)%2 == 1 {
			if t.GetVR() == "UI" {
				b = append(b, 0)
			} else {
				b = append(b, ' ')
			}
		}
		if err := ds.Add(dataelem.NewDataElement(t, dataelem.VR(t.GetVR()), b)); err != nil {
			tb.Fatalf("%s: %v", k, err)
		}
	}
	ts := df.FileMetaInfo.TransferSyntaxUID
	name := filepath.Join(tb.TempDir(), "file.dcm")
	f, err := os.Create(name) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		tb.Fatal(err)
	}
	fw := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	fw.SetFileMetaInfo(&filewriter.FileMetaInfo{
		MediaStorageSOPClassUID:    classOf(tb, df),
		MediaStorageSOPInstanceUID: values["SOPInstanceUID"],
		TransferSyntaxUID:          ts,
	})
	fw.SetValueSource(bytes.NewReader(raw), ts != "1.2.840.10008.1.2.2")
	if err := fw.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		tb.Fatal(err)
	}
	if err := fw.Write(); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
	b, err := os.ReadFile(name) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// classOf returns the SOP Class UID of a file read by go-dicom, its padding
// removed.
func classOf(tb testing.TB, df *filereader.DICOMFile) string {
	tb.Helper()
	el, ok := df.GetDataset().Get(tag.New(0x0008, 0x0016))
	if !ok {
		tb.Fatal("the public file has no SOP Class UID")
	}
	b, ok := el.Bytes()
	if !ok {
		tb.Fatal("the SOP Class UID has no bytes")
	}
	return string(bytes.TrimRight(b, "\x00 "))
}
