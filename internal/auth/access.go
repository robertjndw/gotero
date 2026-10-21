package auth

import "github.com/robertjndw/gotero/internal/repo"

// LibraryAccess is what a principal may do in one library.
type LibraryAccess struct {
	Read      bool
	Write     bool
	Notes     bool
	FileRead  bool
	FileWrite bool
}

// UserLibraryAccess applies the key's flags to its owner's own library.
// Other users' personal libraries are never accessible.
func UserLibraryAccess(p Principal, ownerID int64) LibraryAccess {
	if p.User.ID != ownerID {
		return LibraryAccess{}
	}
	a := p.Key.Access
	return LibraryAccess{
		Read:      a.Library,
		Write:     a.Library && a.Write,
		Notes:     a.Library && a.Notes,
		FileRead:  a.Library && a.Files,
		FileWrite: a.Library && a.Files && a.Write,
	}
}

// GroupLibraryAccess combines the group's settings, the caller's role
// ("owner", "admin", "member" or "" for non-members) and the key's group
// permission ("all" = read/write, "read", "none").
func GroupLibraryAccess(p Principal, g repo.Group, role string) LibraryAccess {
	keyGroups := p.Key.Access.Groups
	if keyGroups != "all" && keyGroups != "read" {
		return LibraryAccess{}
	}
	member := role != ""
	admin := role == "owner" || role == "admin"
	if !member && g.LibraryReading != "all" {
		return LibraryAccess{}
	}
	keyWrite := keyGroups == "all"
	write := keyWrite && (admin || (member && g.LibraryEditing == "members"))
	fileWrite := write && (g.FileEditing == "members" || (admin && g.FileEditing == "admins"))
	return LibraryAccess{
		Read:      true,
		Write:     write,
		Notes:     true,
		FileRead:  true,
		FileWrite: fileWrite,
	}
}
