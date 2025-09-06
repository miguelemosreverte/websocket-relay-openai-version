package main

import (
    "encoding/base64"
    "flag"
    "fmt"
    "io"
    "os"

    "golang.org/x/crypto/nacl/box"
)

// This program implements GitHub Actions secret encryption (libsodium sealed box).
// Usage: echo -n "secret" | go run ./scripts/gh_seal.go -pub <base64PublicKey>
// Output: base64 ciphertext suitable for GitHub API encrypted_value.

func main() {
    pubB64 := flag.String("pub", "", "GitHub public key (base64)")
    flag.Parse()
    if *pubB64 == "" {
        fmt.Fprintln(os.Stderr, "-pub (base64 public key) is required")
        os.Exit(2)
    }
    pubRaw, err := base64.StdEncoding.DecodeString(*pubB64)
    if err != nil || len(pubRaw) != 32 {
        fmt.Fprintln(os.Stderr, "invalid public key")
        os.Exit(2)
    }
    var recipient [32]byte
    copy(recipient[:], pubRaw)

    // Read plaintext from stdin
    plaintext, err := io.ReadAll(os.Stdin)
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }

    // Use sealed box implementation from x/crypto/nacl/box (SealAnonymous).
    sealed, err := box.SealAnonymous(nil, plaintext, &recipient, nil)
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    fmt.Print(base64.StdEncoding.EncodeToString(sealed))
}
