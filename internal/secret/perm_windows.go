//go:build windows

package secret

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Windows gives a file an access list (a DACL: discretionary access control
// list) rather than mode bits, and os.Chmod there only sets the read-only
// attribute, so the narrowest permission must be written and read through the
// Win32 security functions. The syscall package exports the token and SID
// functions but not those that build or read a DACL, so the four used here
// are taken from advapi32.dll through syscall.NewLazyDLL. The syscall package
// registers advapi32.dll as a system DLL, so it is loaded only from the
// System32 directory, never from a directory an attacker can write to. This
// is why golang.org/x/sys/windows is not needed.
var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityInfo                                      = advapi32.NewProc("GetSecurityInfo")
	procGetAce                                               = advapi32.NewProc("GetAce")
)

// Values from the Windows SDK (winnt.h, accctrl.h, sddl.h).
const (
	sddlRevision1             = 1
	seFileObject              = 1 // SE_OBJECT_TYPE: a file or a directory
	ownerSecurityInformation  = 0x1
	daclSecurityInformation   = 0x4
	accessAllowedACEType      = 0x0
	accessDeniedACEType       = 0x1
	systemAuditACEType        = 0x2
	systemAlarmACEType        = 0x3
	accessDeniedObjectACEType = 0x6
	systemMandatoryLabelType  = 0x11
	inheritOnlyACE            = 0x8
)

// acl is the header of an ACL structure (winnt.h).
type acl struct {
	AclRevision byte
	Sbz1        byte
	AclSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

// aceHeader is the header every ACE starts with (winnt.h). In an
// ACCESS_ALLOWED_ACE it is followed by the 4-byte access mask, then the SID.
type aceHeader struct {
	AceType  byte
	AceFlags byte
	AceSize  uint16
}

// allowedACESIDOffset is where the SID starts in an ACCESS_ALLOWED_ACE: after
// the 4-byte header and the 4-byte access mask.
const allowedACESIDOffset = 8

// currentUserSID returns the SID of the user this process runs as, in its
// string form (S-1-5-21-...).
func currentUserSID() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() { _ = token.Close() }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

// createPrivate creates a new file at path whose access list allows the
// current user full control and names nobody else. The DACL is protected
// ("P"), so the entries the directory would pass on (SYSTEM and
// Administrators, usually) are not inherited, and the owner is set to the
// current user, because an owner may always rewrite the DACL and an elevated
// process would otherwise make the Administrators group the owner. The
// security descriptor is given to CreateFile, so the file never exists with
// a wider access list, not even for a moment. CREATE_NEW fails if anything
// exists at path.
func createPrivate(path string) (*os.File, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sddl, err := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	var sd uintptr
	r, _, callErr := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return nil, callErr
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(sd)) }()

	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_WRITE, 0, &sa,
		syscall.CREATE_NEW, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// fileSecurity returns f's owner and the SID of every entry in its access
// list that grants access and applies to the file itself, in string form. A
// null DACL, which grants everyone everything, and a granting entry of any
// type other than ACCESS_ALLOWED_ACE (an object or callback entry, which a
// file does not normally carry), are returned as an accessError, since
// neither can be shown to name only one user.
func fileSecurity(f *os.File) (owner string, sids []string, err error) {
	var ownerSID *syscall.SID
	var dacl *acl
	var sd uintptr
	// GetSecurityInfo returns its error code rather than setting the last
	// error. The handle came from os.Open, whose GENERIC_READ includes the
	// READ_CONTROL right this call needs.
	r, _, _ := procGetSecurityInfo.Call(f.Fd(), seFileObject, ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&ownerSID)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r != 0 {
		return "", nil, syscall.Errno(r)
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(sd)) }()
	if ownerSID == nil {
		return "", nil, accessError("the file has no owner")
	}
	if owner, err = ownerSID.String(); err != nil {
		return "", nil, err
	}
	if dacl == nil {
		return owner, nil, accessError("the file has no access list, which gives every user full access")
	}

	for i := range uint32(dacl.AceCount) {
		var ace *aceHeader
		r, _, callErr := procGetAce.Call(uintptr(unsafe.Pointer(dacl)), uintptr(i), uintptr(unsafe.Pointer(&ace)))
		if r == 0 {
			return owner, nil, callErr
		}
		if ace.AceFlags&inheritOnlyACE != 0 {
			// An inherit-only entry is passed on to children and grants
			// nothing on this object; a file has no children.
			continue
		}
		switch ace.AceType {
		case accessAllowedACEType:
			sid := (*syscall.SID)(unsafe.Add(unsafe.Pointer(ace), allowedACESIDOffset))
			s, err := sid.String()
			if err != nil {
				return owner, nil, err
			}
			sids = append(sids, s)
		case accessDeniedACEType, accessDeniedObjectACEType, systemAuditACEType, systemAlarmACEType, systemMandatoryLabelType:
			// These grant nothing.
		default:
			return owner, nil, accessError("the access list has an entry of type " + strconv.Itoa(int(ace.AceType)) + ", which may grant access")
		}
	}
	return owner, sids, nil
}

// accessError is an access list that fileSecurity cannot reduce to a list of
// users, as opposed to a failure to read the list at all.
type accessError string

// Error returns the reason the access list is not acceptable.
func (e accessError) Error() string { return string(e) }

// openKeyFile opens path for reading. Windows has no FIFO in the file
// system (a named pipe lives in the pipe namespace, not at a file path), so
// a plain open does not block on one here. checkPrivate still refuses
// anything that is not a regular file.
func openKeyFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the path is the operator's own configuration, read on purpose.
}

// checkDir does not check the directory on Windows, in this version: the
// directory's access list is not read. The key file's own owner and access
// list are checked by checkPrivate.
func (fileSystem) checkDir(string, string) error {
	return nil
}

// checkPrivate refuses a key file that is not a regular file, whose owner is
// not the current user, or whose access list grants access to anyone but the
// current user: another account, a group such as Administrators or Users, or
// SYSTEM. It is the Windows counterpart of owner-only mode 0600. The owner
// counts because an owner may always rewrite the access list, whatever it
// says now.
//
// The refusal names the wrong owner and every other SID the list grants, and
// gives the icacls commands that fix exactly those, because an entry may be
// explicit rather than inherited (a file made by os.WriteFile in the
// temporary directory carries explicit SYSTEM and Administrators entries),
// and /inheritance:r alone leaves explicit entries in place.
func (fileSystem) checkPrivate(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be checked")
	}
	if !info.Mode().IsRegular() {
		return fault.New(fault.Invalid, "secret: the key file "+path+" is not a regular file")
	}
	user, err := currentUserSID()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the current user cannot be found to check the key file "+path)
	}
	owner, sids, err := fileSecurity(f)
	var ae accessError
	if errors.As(err, &ae) {
		return fault.New(fault.Invalid, "secret: the key file "+path+" may be opened by other users ("+string(ae)+
			"); make it private to you with: "+renderCommands(fixCommands(path, user, owner != user, nil))+
			", then remove the entry icacls shows for anyone else")
	}
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the access list of the key file "+path+" cannot be read")
	}
	var others []string
	for _, s := range sids {
		if s != user && !slices.Contains(others, s) {
			others = append(others, s)
		}
	}
	var problems []string
	if owner != user {
		problems = append(problems, "is owned by "+owner+", not by the current user "+user)
	}
	if len(others) > 0 {
		problems = append(problems, "may be opened by "+strings.Join(others, ", ")+", not only by the current user")
	}
	if len(sids) == 0 {
		problems = append(problems, "grants access to nobody, not even the current user")
	}
	if len(problems) > 0 {
		return fault.New(fault.Invalid, "secret: the key file "+path+" "+strings.Join(problems, ", and ")+
			"; make it private to you with: "+renderCommands(fixCommands(path, user, owner != user, others)))
	}
	return nil
}

// fixCommands returns the arguments of the icacls commands that make path
// private to user: make user the owner when setOwner is true, drop inherited
// entries and give user full control, then remove each SID in others. icacls
// takes a SID written as *S-1-....
func fixCommands(path, user string, setOwner bool, others []string) [][]string {
	var cmds [][]string
	if setOwner {
		cmds = append(cmds, []string{path, "/setowner", "*" + user})
	}
	cmds = append(cmds, []string{path, "/inheritance:r", "/grant:r", "*" + user + ":F"})
	if len(others) > 0 {
		remove := []string{path, "/remove"}
		for _, s := range others {
			remove = append(remove, "*"+s)
		}
		cmds = append(cmds, remove)
	}
	return cmds
}

// renderCommands writes icacls commands as a person would type them, with the
// path quoted, joined by "then".
func renderCommands(cmds [][]string) string {
	var parts []string
	for _, args := range cmds {
		parts = append(parts, `icacls "`+args[0]+`" `+strings.Join(args[1:], " "))
	}
	return strings.Join(parts, ", then ")
}

// syncDir does nothing on Windows: a directory cannot be opened for
// FlushFileBuffers without backup privileges, and NTFS journals the new
// directory entry itself.
func syncDir(string) error {
	return nil
}

// checkPrivateDir does not check the directory on Windows: its access list is
// not read here, and a file made by CreatePrivateTemp carries its own,
// protected, access list that checkPrivate verifies.
func (fileSystem) checkPrivateDir(string, bool) error { return nil }
