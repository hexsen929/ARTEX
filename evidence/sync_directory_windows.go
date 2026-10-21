package evidence

// Windows does not support FlushFileBuffers on read-only directory handles.
// The body is flushed before rename; directory durability across sudden power
// loss is platform dependent. Do not discard errors from the body flush itself.
func syncEvidenceDirectory(string) error { return nil }
