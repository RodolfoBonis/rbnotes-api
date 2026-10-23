//go:build unix

package claude

import (
	"os/exec"
	"syscall"
	"time"
)

// waitDelay dá um respiro entre o SIGKILL ao grupo e a desistência do Wait: cobre processos
// filhos que demoram a morrer sem travar o worker indefinidamente.
const waitDelay = 10 * time.Second

// setProcessGroup coloca a CLI num grupo de processo próprio (Setpgid) e, no cancelamento do
// contexto (timeout), mata o GRUPO inteiro (-pgid) — a CLI do Claude lança subprocessos (node,
// git, testes) que de outra forma sobreviveriam ao kill do processo líder. cmd.WaitDelay é o
// backstop caso algum filho ignore o sinal.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Sinal negativo = grupo de processo (pgid == pid do líder, por causa do Setpgid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
}
