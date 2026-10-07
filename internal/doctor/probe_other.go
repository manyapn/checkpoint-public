//go:build !linux

package doctor

func kernelCheck() Check {
	return Check{Name: "kernel", Fatal: true, Detail: "checkpoint is Linux-only (fanotify)",
		Remedy: "run it on Linux, or in a privileged container: scripts/linux.sh"}
}

func fanotifyCheck(root string) Check {
	return Check{Name: "fanotify capability", Fatal: true, Detail: "not available on this OS"}
}

func filesystemCheck(root string) Check {
	return Check{Name: "workspace filesystem", Detail: "not probed on this OS"}
}
