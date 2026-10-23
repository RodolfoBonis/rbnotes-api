//go:build !unix

package claude

import "os/exec"

// setProcessGroup é no-op fora de Unix. Os workers rodam em Linux (imagem node:slim); este
// fallback existe só para o código compilar em outras plataformas de desenvolvimento.
func setProcessGroup(_ *exec.Cmd) {}
