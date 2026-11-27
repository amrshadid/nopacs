package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LayoutBagIt is the export layout of RFC 8493: the files under data/, with a
// SHA-256 manifest and the tag files that describe the bag, so another team
// can check the whole set with any BagIt tool.
const LayoutBagIt = "bagit"

// bagPayloadDir is the directory of a bag that holds the payload.
const bagPayloadDir = "data"

// BagFile is one payload file of a bag.
type BagFile struct {
	// Path is the file's path from the bag's directory, with slashes, and
	// starts with "data/".
	Path string
	// SHA256 is its checksum in lowercase hexadecimal.
	SHA256 string
	// Bytes is its length.
	Bytes int64
}

// bagInfo holds the metadata a bag-info.txt states.
type bagInfo struct {
	date        time.Time
	description string
}

// writeBag writes the four files of a bag into dir: bagit.txt, bag-info.txt,
// manifest-sha256.txt and, last, tagmanifest-sha256.txt, which lists the
// SHA-256 of the first three (RFC 8493 2.2.1, 2.2.2, 2.1.3, 2.2.4). Each is
// written whole or not at all, in that order, so a bag is complete only
// when its tag manifest is there. The payload is expected to be in dir already.
func writeBag(dir string, files []BagFile, info bagInfo) error {
	sorted := append([]BagFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var manifest strings.Builder
	var octets int64
	for _, f := range sorted {
		if !strings.HasPrefix(f.Path, bagPayloadDir+"/") {
			return fmt.Errorf("a bag's payload file must be under %s/: %q", bagPayloadDir, f.Path)
		}
		// RFC 8493 2.1.3: a path is written with "%", CR and LF
		// percent-encoded, and a checksum is followed by whitespace.
		manifest.WriteString(f.SHA256 + "  " + encodeBagPath(f.Path) + "\n")
		octets += f.Bytes
	}
	tags := []struct{ name, text string }{
		{"manifest-sha256.txt", manifest.String()},
		{"bagit.txt", "BagIt-Version: 1.0\nTag-File-Character-Encoding: UTF-8\n"},
		{"bag-info.txt", "Bag-Software-Agent: nopacs\n" +
			"Bagging-Date: " + info.date.UTC().Format("2006-01-02") + "\n" +
			"External-Description: " + oneLine(info.description) + "\n" +
			"Payload-Oxum: " + strconv.FormatInt(octets, 10) + "." + strconv.Itoa(len(sorted)) + "\n"},
	}
	var tagManifest strings.Builder
	for _, t := range tags {
		if err := writeAtomic(filepath.Join(dir, t.name), []byte(t.text)); err != nil {
			return fmt.Errorf("cannot write %s: %w", t.name, err)
		}
	}
	// The tag manifest lists the tag files in a fixed order, so the same bag
	// always has the same one.
	for _, name := range []string{"bag-info.txt", "bagit.txt", "manifest-sha256.txt"} {
		for _, t := range tags {
			if t.name == name {
				sum := sha256.Sum256([]byte(t.text))
				tagManifest.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
			}
		}
	}
	if err := writeAtomic(filepath.Join(dir, "tagmanifest-sha256.txt"), []byte(tagManifest.String())); err != nil {
		return fmt.Errorf("cannot write tagmanifest-sha256.txt: %w", err)
	}
	return nil
}

// encodeBagPath percent-encodes the characters RFC 8493 2.1.3 says a manifest
// path must not hold as they are.
func encodeBagPath(p string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(p)
}

// oneLine turns text into a single line, since a bag-info.txt value
// continues on the next line only when that starts with whitespace.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
