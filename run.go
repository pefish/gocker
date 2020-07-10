package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func createContainerID() string {
	randBytes := make([]byte, 6)
	rand.Read(randBytes)
	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x",
		randBytes[0], randBytes[1], randBytes[2],
		randBytes[3], randBytes[4], randBytes[5])
}

func getContainerFSHome(contanerID string) string {
	return getGockerContainersPath() + "/" + contanerID + "/fs"
}

func createContainerDirectories(containerID string) {
	contHome := getGockerContainersPath() + "/" + containerID
	contDirs := []string{contHome + "/fs", contHome + "/fs/mnt", contHome + "/fs/upperdir", contHome + "/fs/workdir"}
	if err := createDirsIfDontExist(contDirs); err != nil {
		log.Fatalf("Unable to create required directories: %v\n", err)
	}
}

func mountOverlayFileSystem(containerID string, imageShaHex string) {
	var srcLayers []string
	pathManifest := getManifestPathForImage(imageShaHex)
	mani := manifest{}
	parseManifest(pathManifest, &mani)
	if len(mani) == 0 || len(mani[0].Layers) == 0 {
		log.Fatal("Could not find any layers.")
	}
	if len(mani) > 1 {
		log.Fatal("I don't know how to handle more than one manifest.")
	}

	imageBasePath := getBasePathForImage(imageShaHex)
	for _, layer := range mani[0].Layers {
		srcLayers = append([]string{imageBasePath + "/" + layer[:12] + "/fs"}, srcLayers...)
		//srcLayers = append(srcLayers, imageBasePath + "/" + layer[:12] + "/fs")
	}
	contFSHome := getContainerFSHome(containerID)
	mntOptions := "lowerdir=" + strings.Join(srcLayers, ":") + ",upperdir=" + contFSHome + "/upperdir,workdir=" + contFSHome + "/workdir"
	if err := syscall.Mount("none", contFSHome+"/mnt", "overlay", 0, mntOptions); err != nil {
		log.Fatalf("Mount failed: %v\n", err)
	}
}

func unmountNetworkNamespace(containerID string) {
	netNsPath := getGockerNetNsPath() + "/" + containerID
	if err := syscall.Unmount(netNsPath, 0); err != nil {
		log.Fatalf("Uable to mount network namespace: %v at %s", err, netNsPath)
	}
}

func unmountContainerFs(containerID string) {
	mountedPath := getGockerContainersPath() + "/" + containerID + "/fs/mnt"
	if err := syscall.Unmount(mountedPath, 0); err != nil {
		log.Fatalf("Uable to mount container file system: %v at %s", err, mountedPath)
	}
}

func copyNameserverConfig(containerID string) error {
	resolvFilePaths := []string{
		"/var/run/systemd/resolve/resolv.conf",
		"/etc/gockerresolv.conf",
		"/etc/resolv.conf",
	}
	for _, resolvFilePath := range resolvFilePaths {
		if _, err := os.Stat(resolvFilePath); os.IsNotExist(err) {
			continue
		} else {
			return copyFile(resolvFilePath,
				getContainerFSHome(containerID)+"/mnt/etc/resolv.conf")
		}
	}
	return nil
}

/*
	Called if this program is executed with "child-mode" as the first argument
*/
func execContainerCommand(mem int, swap int, pids int, cpus float64,
	containerID string, imageShaHex string, args []string) {
	mntPath := getContainerFSHome(containerID) + "/mnt"
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	imgConfig := parseContainerConfig(imageShaHex)
	doOrDieWithMsg(syscall.Sethostname([]byte(containerID)), "Unable to set hostname")  // 设置容器的hostname（命名空间的关系，只会作用于容器）
	doOrDieWithMsg(joinContainerNetworkNamespace(containerID), "Unable to join container network namespace")  // 激活网络命名空间
	createCGroups(containerID, true)  // 创建cpu、mem等各种cgroup
	configureCGroups(containerID, mem, swap, pids, cpus)  // 根据设置的最大用量配置cgroup
	doOrDieWithMsg(copyNameserverConfig(containerID), "Unable to copy resolve.conf")  // 将主机中dns的配置复制到容器
	doOrDieWithMsg(syscall.Chroot(mntPath), "Unable to chroot")  // 设置容器的文件系统根目录
	doOrDieWithMsg(os.Chdir("/"), "Unable to change directory")  // 进入根目录
	createDirsIfDontExist([]string{"/proc", "/sys"})  // 容器中如果不存在/proc和/sys目录，则创建他们
	doOrDieWithMsg(syscall.Mount("proc", "/proc", "proc", 0, ""), "Unable to mount proc")  // 挂载目录/proc。-v的作用
	doOrDieWithMsg(syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, ""), "Unable to mount tmpfs")  // 挂载目录/tmp
	doOrDieWithMsg(syscall.Mount("tmpfs", "/dev", "tmpfs", 0, ""), "Unable to mount tmpfs on /dev") // 挂载目录/dev
	createDirsIfDontExist([]string{"/dev/pts"})  // /dev/pts目录不存在的话就创建
	doOrDieWithMsg(syscall.Mount("devpts", "/dev/pts", "devpts", 0, ""), "Unable to mount devpts")  // 挂载目录/dev/pts
	doOrDieWithMsg(syscall.Mount("sysfs", "/sys", "sysfs", 0, ""), "Unable to mount sysfs")  // 挂载目录/sys
	setupLocalInterface()  // 设置容器网络命名空间的lo回环网卡
	cmd.Env = imgConfig.Config.Env
	cmd.Run()  // 执行命令
	doOrDie(syscall.Unmount("/dev/pts", 0))  // 命令执行完后卸载所有共享目录
	doOrDie(syscall.Unmount("/dev", 0))
	doOrDie(syscall.Unmount("/sys", 0))
	doOrDie(syscall.Unmount("/proc", 0))
	doOrDie(syscall.Unmount("/tmp", 0))
}

func prepareAndExecuteContainer(mem int, swap int, pids int, cpus float64,
	containerID string, imageShaHex string, cmdArgs []string) {

	/* Setup the network namespace  */
	cmd := &exec.Cmd{
		Path:   "/proc/self/exe",
		Args:   []string{"/proc/self/exe", "setup-netns", containerID},  // linux中/proc/self/exe指代当前程序，也就是gocker
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	cmd.Run()

	/* Namespace and setup the virtual interface  */
	cmd = &exec.Cmd{
		Path:   "/proc/self/exe",
		Args:   []string{"/proc/self/exe", "setup-veth", containerID},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	cmd.Run()
	/*
		From namespaces(7)
		       Namespace Flag            Isolates
		       --------- ----   		 --------
		       Cgroup    CLONE_NEWCGROUP Cgroup root directory
		       IPC       CLONE_NEWIPC    System V IPC,
		                                 POSIX message queues
		       Network   CLONE_NEWNET    Network devices,
		                                 stacks, ports, etc.
		       Mount     CLONE_NEWNS     Mount points
		       PID       CLONE_NEWPID    Process IDs
		       Time      CLONE_NEWTIME   Boot and monotonic
		                                 clocks
		       User      CLONE_NEWUSER   User and group IDs
		       UTS       CLONE_NEWUTS    Hostname and NIS
		                                 domain name
	*/
	var opts []string
	if mem > 0 {
		opts = append(opts, "--mem="+strconv.Itoa(mem))
	}
	if swap >= 0 {
		opts = append(opts, "--swap="+strconv.Itoa(swap))
	}
	if pids > 0 {
		opts = append(opts, "--pids="+strconv.Itoa(pids))
	}
	if cpus > 0 {
		opts = append(opts, "--cpus="+strconv.FormatFloat(cpus, 'f', 1, 64))
	}
	opts = append(opts, "--img="+imageShaHex)
	args := append([]string{containerID}, cmdArgs...)
	args = append(opts, args...)
	args = append([]string{"child-mode"}, args...)
	cmd = exec.Command("/proc/self/exe", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC,
	}
	doOrDie(cmd.Run())
}

func initContainer(mem int, swap int, pids int, cpus float64, src string, args []string) {
	containerID := createContainerID()  // 随机生成一个容器id
	log.Printf("New container ID: %s\n", containerID)
	imageShaHex := downloadImageIfRequired(src)  // 获取镜像hash
	log.Printf("Image to overlay mount: %s\n", imageShaHex)
	createContainerDirectories(containerID)  // 创建容器相关的目录
	mountOverlayFileSystem(containerID, imageShaHex)  // 挂载文件系统，安装FS命名空间
	if err := setupVirtualEthOnHost(containerID); err != nil {  // 主机中安装vth0的虚拟网卡，并挂载到bridge交换机设备
		log.Fatalf("Unable to setup Veth0 on host: %v", err)
	}
	prepareAndExecuteContainer(mem, swap, pids, cpus, containerID, imageShaHex, args)  // 配置命名空间->配置容器内网卡->执行命令
	log.Printf("Container done.\n")
	unmountNetworkNamespace(containerID)  // 卸载网络命名空间
	unmountContainerFs(containerID)  // 卸载FS命名空间
	removeCGroups(containerID)  // 移除cgroups
	os.RemoveAll(getGockerContainersPath() + "/" + containerID)  // 清理掉容器目录
}
