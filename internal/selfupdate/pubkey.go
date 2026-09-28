package selfupdate

// publicKey is the base64 Ed25519 public key that release manifests must be
// signed with (`go run ./cmd/releasekey pub`). The private key never enters
// the repository: C:\Users\Morgott\.config\issuewatcher\release-ed25519.key on
// the release machine, owner-only. A var so a test build can swap it via
// -ldflags "-X github.com/UberMorgott/issuewatcher/internal/selfupdate.publicKey=...".
var publicKey = "Vpya7TaMI5ll9v3a4nCCbGJez0VUvL/lbDk5Nj23eX8="
