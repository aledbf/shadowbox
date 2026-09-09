package tests

// The Go standard library's syscall package does not wrap every call
// this suite needs on linux/amd64, so the handful that are missing are
// issued directly.  The numbers are the amd64 ABI's and are fixed for
// the life of the architecture.
const (
	sysGetcpu    = 309
	sysSetitimer = 38

	itimerReal = 0
)

type timeval struct{ Sec, Usec int64 }

type itimerval struct{ Interval, Value timeval }
