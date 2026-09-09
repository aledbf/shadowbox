package tests

import "errors"

// A one line indirection so the rest of the package reads the same as it
// would with errors.As inlined; kept separate because several files use
// it and none of them should have to import errors for one call.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
