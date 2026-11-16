package claudecode

// Version is the pinned Claude Code release. It moves only after the
// conformance suite passes against it.
const Version = "2.1.325"

// SHA512 holds the hex sha512 of each platform's npm tarball at Version, by
// GOARCH. Each is the registry's dist.integrity for the release, decoded from
// base64.
var SHA512 = map[string]string{
	"amd64": "06a67332f3ffce02cb2f7e12fbb3b36c6f9cca8169ee0c161d4e64a15c2cc8c953033fb4cd90266d8f94933bed376457c69e8070c7a76d8223409c685df518df",
	"arm64": "342bd070ec6bebce58e26918523413f25fcebb3c34936c5696859ecb37ec52d5a239e747a2c3e4696e97092a73025d5d32ebc334117deeaa568150eb49d2b657",
}
