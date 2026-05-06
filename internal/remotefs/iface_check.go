package remotefs

// Compile-time interface check for the impls in this package.
var (
	_ Store = (*FileStore)(nil)
	_ Store = (*GoogleDrive)(nil)
)
