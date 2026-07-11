package remotefs

// Compile-time interface check for the impls in this package.
var (
	_ Store = (*FileStore)(nil)
	_ Store = (*GoogleDrive)(nil)
	_ Store = (*GoogleCloudStorage)(nil)
	_ Store = (*S3)(nil)
	_ Store = (*AzureBlob)(nil)
	_ Store = (*External)(nil)
)
