//go:build darwin

package secret

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// macOS keeps a file's access list apart from its mode bits: after
// chmod +a "everyone allow read" a file still shows mode 0600, and anyone may
// read it. The standard library has no call that reads the list, and the C
// library's acl_get_fd would need cgo, so permitACL asks the kernel directly
// with fgetattrlist(2) for ATTR_CMN_EXTENDED_SECURITY, through
// syscall.Syscall6, which the syscall package provides on darwin without
// cgo. The layouts below are those of <sys/attr.h> and <sys/kauth.h>.
const (
	attrBitMapCount          = 5           // ATTR_BIT_MAP_COUNT
	attrCmnReturnedAttrs     = 0x80000000  // ATTR_CMN_RETURNED_ATTRS
	attrCmnExtendedSecurity  = 0x00400000  // ATTR_CMN_EXTENDED_SECURITY
	kauthFilesecMagic        = 0x012cc16d  // KAUTH_FILESEC_MAGIC
	kauthFilesecNoACL        = 0xffffffff  // KAUTH_FILESEC_NOACL
	kauthACEKindMask         = 0xf         // KAUTH_ACE_KINDMASK
	kauthACEPermit           = 1           // KAUTH_ACE_PERMIT
	kauthFilesecHeaderSize   = 4 + 16 + 16 // magic, owner GUID, group GUID
	kauthACLHeaderSize       = 4 + 4       // entry count, flags
	kauthACESize             = 16 + 4 + 4  // GUID, flags, rights
	attrReplyFixedSize       = 4 + 5*4     // length, attribute_set_t
	attrReferenceSize        = 4 + 4       // attrreference_t: offset, length
	extendedSecurityMaxReply = 64 << 10
)

// attrList is struct attrlist from <sys/attr.h>.
type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

// errACLReply is a reply from fgetattrlist that does not have the documented
// layout. It is an error, not "no access list", so a layout this code does
// not understand refuses the file instead of passing it.
var errACLReply = errors.New("the kernel's access list reply is not in the expected layout")

// permitACL reports whether f carries an access list entry that grants
// (permits) anything. An entry that only denies is not counted: it takes
// access away and cannot open the file to anyone.
func permitACL(f *os.File) (bool, error) {
	al := attrList{bitmapCount: attrBitMapCount, commonAttr: attrCmnReturnedAttrs | attrCmnExtendedSecurity}
	buf := make([]byte, extendedSecurityMaxReply)
	_, _, errno := syscall.Syscall6(syscall.SYS_FGETATTRLIST, f.Fd(),
		uintptr(unsafe.Pointer(&al)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return false, errno
	}
	return parseACLReply(buf)
}

// parseACLReply reads the reply of fgetattrlist for ATTR_CMN_RETURNED_ATTRS
// and ATTR_CMN_EXTENDED_SECURITY. The reply holds its length, the set of
// attributes returned and, when the file has an access list, a reference
// (offset from the reference itself, and length) to a kauth_filesec: a magic
// number, two GUIDs, the entry count and the entries. All in host order.
func parseACLReply(buf []byte) (bool, error) {
	le := binary.LittleEndian
	if len(buf) < attrReplyFixedSize {
		return false, errACLReply
	}
	length := int(le.Uint32(buf))
	if length < attrReplyFixedSize || length > len(buf) {
		return false, errACLReply
	}
	buf = buf[:length]
	if le.Uint32(buf[4:])&attrCmnExtendedSecurity == 0 {
		// The attribute was not returned: the file has no access list.
		return false, nil
	}
	ref := attrReplyFixedSize
	if len(buf) < ref+attrReferenceSize {
		return false, errACLReply
	}
	offset := int(int32(le.Uint32(buf[ref:]))) //nolint:gosec // G115: attrreference_t's offset is an int32 by definition.
	size := int(le.Uint32(buf[ref+4:]))
	start := ref + offset
	if offset < 0 || size < 0 || start > len(buf) || size > len(buf)-start {
		return false, errACLReply
	}
	sec := buf[start : start+size]
	if len(sec) < kauthFilesecHeaderSize+kauthACLHeaderSize || le.Uint32(sec) != kauthFilesecMagic {
		return false, errACLReply
	}
	count := le.Uint32(sec[kauthFilesecHeaderSize:])
	if count == kauthFilesecNoACL {
		return false, nil
	}
	entries := sec[kauthFilesecHeaderSize+kauthACLHeaderSize:]
	if uint64(count)*kauthACESize > uint64(len(entries)) {
		return false, errACLReply
	}
	for i := range int(count) {
		flags := le.Uint32(entries[i*kauthACESize+16:])
		if flags&kauthACEKindMask == kauthACEPermit {
			return true, nil
		}
	}
	return false, nil
}
