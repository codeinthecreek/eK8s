package wrapper

// Hasher is the calling interface of the fixed hashing core, and the
// only way to hash or verify a password. This file is pinned: it must
// stay byte-identical to the canonical version.
//
// Both methods are safe for concurrent use. Each call costs one Argon2id
// computation, about 19 MiB of memory and tens of milliseconds of CPU.
// A call cannot be cancelled or interrupted once started (there is no
// context parameter): a disconnected client's in-flight call still runs
// to completion.
type Hasher interface {
	// Hash returns a PHC-encoded Argon2id string at exactly m=19456 KiB,
	// t=2, p=1, with a fresh random 16-byte salt per call and a 32-byte
	// key: "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>". It returns an
	// error only if randomness is unavailable; in practice it never does.
	Hash(password string) (string, error)

	// Verify reports whether password matches the encoded hash: (true,
	// nil) on a match, (false, nil) on a mismatch, never an error for a
	// mismatch. A non-nil error means only that encoded is malformed or
	// not at exactly the parameters above. The comparison is
	// constant-time inside the core.
	Verify(password, encoded string) (bool, error)
}
