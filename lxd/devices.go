package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"

	"github.com/lxc/lxd/shared"
	"gopkg.in/lxc/go-lxc.v2"

	log "gopkg.in/inconshreveable/log15.v2"
)

var deviceSchedRebalance = make(chan []string, 1)

type deviceTaskCPU struct {
	id    int
	strId string
	count *int
}
type deviceTaskCPUs []deviceTaskCPU

func (c deviceTaskCPUs) Len() int           { return len(c) }
func (c deviceTaskCPUs) Less(i, j int) bool { return *c[i].count < *c[j].count }
func (c deviceTaskCPUs) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

func deviceMonitorProcessors() (chan []string, error) {
	NETLINK_KOBJECT_UEVENT := 15
	UEVENT_BUFFER_SIZE := 2048

	fd, err := syscall.Socket(
		syscall.AF_NETLINK, syscall.SOCK_RAW,
		NETLINK_KOBJECT_UEVENT,
	)

	if err != nil {
		return nil, err
	}

	nl := syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Pid:    uint32(os.Getpid()),
		Groups: 1,
	}

	err = syscall.Bind(fd, &nl)
	if err != nil {
		return nil, err
	}

	ch := make(chan []string, 0)

	go func(ch chan []string) {
		b := make([]byte, UEVENT_BUFFER_SIZE*2)
		for {
			_, err := syscall.Read(fd, b)
			if err != nil {
				continue
			}

			props := map[string]string{}
			last := 0
			for i, e := range b {
				if i == len(b) || e == 0 {
					msg := string(b[last+1 : i])
					last = i
					if len(msg) == 0 || msg == "\x00" {
						continue
					}

					fields := strings.SplitN(msg, "=", 2)
					if len(fields) != 2 {
						continue
					}

					props[fields[0]] = fields[1]
				}
			}

			if props["SUBSYSTEM"] != "cpu" || props["DRIVER"] != "processor" {
				continue
			}

			if props["ACTION"] != "offline" && props["ACTION"] != "online" {
				continue
			}

			ch <- []string{path.Base(props["DEVPATH"]), props["ACTION"]}
		}
	}(ch)

	return ch, nil
}

func deviceTaskBalance(d *Daemon) {
	min := func(x, y int) int {
		if x < y {
			return x
		}
		return y
	}

	// Count CPUs
	cpus := []int{}
	dents, err := ioutil.ReadDir("/sys/bus/cpu/devices/")
	if err != nil {
		shared.Log.Error("balance: Unable to list CPUs", log.Ctx{"err": err})
		return
	}

	for _, f := range dents {
		id := -1
		count, err := fmt.Sscanf(f.Name(), "cpu%d", &id)
		if count != 1 || id == -1 {
			shared.Log.Error("balance: Bad CPU", log.Ctx{"path": f.Name()})
			continue
		}

		onlinePath := fmt.Sprintf("/sys/bus/cpu/devices/%s/online", f.Name())
		if !shared.PathExists(onlinePath) {
			// CPUs without an online file are non-hotplug so are always online
			cpus = append(cpus, id)
			continue
		}

		online, err := ioutil.ReadFile(onlinePath)
		if err != nil {
			shared.Log.Error("balance: Bad CPU", log.Ctx{"path": f.Name(), "err": err})
			continue
		}

		if online[0] == byte('0') {
			continue
		}

		cpus = append(cpus, id)
	}

	// Iterate through the containers
	containers, err := dbContainersList(d.db, cTypeRegular)
	fixedContainers := map[int][]container{}
	balancedContainers := map[container]int{}
	for _, name := range containers {
		c, err := containerLXDLoad(d, name)
		if err != nil {
			continue
		}

		conf := c.ExpandedConfig()
		cpu, ok := conf["limits.cpu"]
		if !ok || cpu == "" {
			cpu = fmt.Sprintf("%d", len(cpus))
		}

		if !c.IsRunning() {
			continue
		}

		count, err := strconv.Atoi(cpu)
		if err == nil {
			// Load-balance
			count = min(count, len(cpus))
			balancedContainers[c] = count
		} else {
			// Pinned
			chunks := strings.Split(cpu, ",")
			for _, chunk := range chunks {
				if strings.Contains(chunk, "-") {
					// Range
					fields := strings.SplitN(chunk, "-", 2)
					if len(fields) != 2 {
						shared.Log.Error("Invalid limits.cpu value.", log.Ctx{"container": c.Name(), "value": cpu})
						continue
					}

					low, err := strconv.Atoi(fields[0])
					if err != nil {
						shared.Log.Error("Invalid limits.cpu value.", log.Ctx{"container": c.Name(), "value": cpu})
						continue
					}

					high, err := strconv.Atoi(fields[1])
					if err != nil {
						shared.Log.Error("Invalid limits.cpu value.", log.Ctx{"container": c.Name(), "value": cpu})
						continue
					}

					for i := low; i <= high; i++ {
						if !shared.IntInSlice(i, cpus) {
							continue
						}

						_, ok := fixedContainers[i]
						if ok {
							fixedContainers[i] = append(fixedContainers[i], c)
						} else {
							fixedContainers[i] = []container{c}
						}
					}
				} else {
					// Simple entry
					nr, err := strconv.Atoi(chunk)
					if err != nil {
						shared.Log.Error("Invalid limits.cpu value.", log.Ctx{"container": c.Name(), "value": cpu})
						continue
					}

					if !shared.IntInSlice(nr, cpus) {
						continue
					}

					_, ok := fixedContainers[nr]
					if ok {
						fixedContainers[nr] = append(fixedContainers[nr], c)
					} else {
						fixedContainers[nr] = []container{c}
					}
				}
			}
		}
	}

	// Balance things
	pinning := map[container][]string{}
	usage := make(deviceTaskCPUs, 0)

	for _, id := range cpus {
		cpu := deviceTaskCPU{}
		cpu.id = id
		cpu.strId = fmt.Sprintf("%d", id)
		count := 0
		cpu.count = &count

		usage = append(usage, cpu)
	}

	for cpu, ctns := range fixedContainers {
		id := usage[cpu].strId
		for _, ctn := range ctns {
			_, ok := pinning[ctn]
			if ok {
				pinning[ctn] = append(pinning[ctn], id)
			} else {
				pinning[ctn] = []string{id}
			}
			*usage[cpu].count += 1
		}
	}

	for ctn, count := range balancedContainers {
		sort.Sort(usage)
		for _, cpu := range usage {
			if count == 0 {
				break
			}
			count -= 1

			id := cpu.strId
			_, ok := pinning[ctn]
			if ok {
				pinning[ctn] = append(pinning[ctn], id)
			} else {
				pinning[ctn] = []string{id}
			}
			*cpu.count += 1
		}
	}

	// Set the new pinning
	for ctn, set := range pinning {
		sort.Strings(set)
		err := ctn.SetCGroup("cpuset.cpus", strings.Join(set, ","))
		if err != nil {
			shared.Log.Error("balance: Unable to set cpuset", log.Ctx{"name": ctn.Name(), "err": err, "value": strings.Join(set, ",")})
		}
	}
}

func deviceTaskScheduler(d *Daemon) {
	chHotplug, err := deviceMonitorProcessors()
	if err != nil {
		shared.Log.Error("scheduler: couldn't setup uevent watcher, no automatic re-balance")
		return
	}

	shared.Debugf("Scheduler: doing initial balance")
	deviceTaskBalance(d)

	for {
		select {
		case e := <-chHotplug:
			if len(e) != 2 {
				shared.Log.Error("Scheduler: received an invalid hotplug event")
				continue
			}
			shared.Debugf("Scheduler: %s is now %s: re-balancing", e[0], e[1])
			deviceTaskBalance(d)
		case e := <-deviceSchedRebalance:
			if len(e) != 3 {
				shared.Log.Error("Scheduler: received an invalid rebalance event")
				continue
			}
			shared.Debugf("Scheduler: %s %s %s: re-balancing", e[0], e[1], e[2])
			deviceTaskBalance(d)
		}
	}
}

func devGetOptions(d shared.Device) (string, error) {
	opts := []string{"bind", "create=file"}
	if d["uid"] != "" {
		u, err := strconv.Atoi(d["uid"])
		if err != nil {
			return "", err
		}
		opts = append(opts, fmt.Sprintf("uid=%d", u))
	}
	if d["gid"] != "" {
		g, err := strconv.Atoi(d["gid"])
		if err != nil {
			return "", err
		}
		opts = append(opts, fmt.Sprintf("gid=%d", g))
	}
	if d["mode"] != "" {
		m, err := devModeOct(d["mode"])
		if err != nil {
			return "", err
		}
		opts = append(opts, fmt.Sprintf("mode=%0d", m))
	} else {
		opts = append(opts, "mode=0660")
	}

	return strings.Join(opts, ","), nil
}

func modeHasRead(mode int) bool {
	if mode&0444 != 0 {
		return true
	}
	return false
}

func modeHasWrite(mode int) bool {
	if mode&0222 != 0 {
		return true
	}
	return false
}

func devModeOct(strmode string) (int, error) {
	if strmode == "" {
		return 0660, nil
	}
	i, err := strconv.ParseInt(strmode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("Bad device mode: %s", strmode)
	}
	return int(i), nil
}

func devModeString(strmode string) (string, error) {
	i, err := devModeOct(strmode)
	if err != nil {
		return "", err
	}
	mode := "m"
	if modeHasRead(i) {
		mode = mode + "r"
	}
	if modeHasWrite(i) {
		mode = mode + "w"
	}
	return mode, nil
}

func getDev(path string) (int, int, error) {
	stat := syscall.Stat_t{}
	err := syscall.Stat(path, &stat)
	if err != nil {
		return 0, 0, err
	}
	major := int(stat.Rdev / 256)
	minor := int(stat.Rdev % 256)
	return major, minor, nil
}

func deviceCgroupInfo(dev shared.Device) (string, error) {
	var err error

	t := dev["type"]
	switch t {
	case "unix-char":
		t = "c"
	case "unix-block":
		t = "b"
	default: // internal error - look at how we were called
		return "", fmt.Errorf("BUG: bad device type %s", dev["type"])
	}

	var major, minor int
	if dev["major"] == "" && dev["minor"] == "" {
		devname := dev["path"]
		if !filepath.IsAbs(devname) {
			devname = filepath.Join("/", devname)
		}
		major, minor, err = getDev(devname)
		if err != nil {
			return "", err
		}
	} else if dev["major"] != "" && dev["minor"] != "" {
		major, err = strconv.Atoi(dev["major"])
		if err != nil {
			return "", err
		}
		minor, err = strconv.Atoi(dev["minor"])
		if err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("Both major and minor must be supplied for devices")
	}

	mode, err := devModeString(dev["mode"])
	if err != nil {
		return "", err
	}
	devcg := fmt.Sprintf("%s %d:%d %s", t, major, minor, mode)
	return devcg, nil
}

/*
 * unixDevCgroup only grabs the cgroup devices.allow statement
 * we need.  We'll add a mount.entry to bind mount the actual
 * device later.
 */
func unixDevCgroup(dev shared.Device) ([][]string, error) {
	devcg, err := deviceCgroupInfo(dev)
	if err != nil {
		return [][]string{}, err
	}
	entry := []string{"lxc.cgroup.devices.allow", devcg}
	return [][]string{entry}, nil
}

func deviceToLxc(cntPath string, d shared.Device) ([][]string, error) {
	switch d["type"] {
	case "unix-char":
		return unixDevCgroup(d)
	case "unix-block":
		return unixDevCgroup(d)

	case "nic":
		// A few checks
		if d["nictype"] == "" {
			return nil, fmt.Errorf("Missing nic type")
		}

		if !shared.StringInSlice(d["nictype"], []string{"bridged", "physical", "p2p", "macvlan"}) {
			return nil, fmt.Errorf("Bad nic type: %s", d["nictype"])
		}

		if shared.StringInSlice(d["nictype"], []string{"bridged", "physical", "macvlan"}) && d["parent"] == "" {
			return nil, fmt.Errorf("Missing parent for %s type nic.", d["nictype"])
		}

		// Generate the LXC config
		var line []string
		var lines = [][]string{}

		if shared.StringInSlice(d["nictype"], []string{"bridged", "p2p"}) {
			line = []string{"lxc.network.type", "veth"}
			lines = append(lines, line)
		} else if d["nictype"] == "physical" {
			line = []string{"lxc.network.type", "phys"}
			lines = append(lines, line)
		} else if d["nictype"] == "macvlan" {
			line = []string{"lxc.network.type", "macvlan"}
			lines = append(lines, line)

			line = []string{"lxc.network.macvlan.mode", "bridge"}
			lines = append(lines, line)
		}

		if d["hwaddr"] != "" {
			line = []string{"lxc.network.hwaddr", d["hwaddr"]}
			lines = append(lines, line)
		}

		if d["mtu"] != "" {
			line = []string{"lxc.network.mtu", d["mtu"]}
			lines = append(lines, line)
		}

		if shared.StringInSlice(d["nictype"], []string{"bridged", "physical", "macvlan"}) {
			line = []string{"lxc.network.link", d["parent"]}
			lines = append(lines, line)
		}

		if d["name"] != "" {
			line = []string{"lxc.network.name", d["name"]}
			lines = append(lines, line)
		}

		return lines, nil
	case "disk":
		var p string
		configLines := [][]string{}
		if d["path"] == "/" || d["path"] == "" {
			p = ""
		} else if d["path"][0:1] == "/" {
			p = d["path"][1:]
		} else {
			p = d["path"]
		}
		source := d["source"]
		options := []string{}
		if shared.IsBlockdevPath(d["source"]) {
			l, err := mountTmpBlockdev(cntPath, d)
			if err != nil {
				return nil, fmt.Errorf("Error adding blockdev: %s", err)
			}
			configLines = append(configLines, l)
			return configLines, nil
		} else if shared.IsDir(source) {
			options = append(options, "bind")
			options = append(options, "create=dir")
		} else /* file bind mount */ {
			/* Todo - can we distinguish between file bind mount and
			 * a qcow2 (or other fs container) file? */
			options = append(options, "bind")
			options = append(options, "create=file")
		}
		if d["readonly"] == "1" || d["readonly"] == "true" {
			options = append(options, "ro")
		}
		if d["optional"] == "1" || d["optional"] == "true" {
			options = append(options, "optional")
		}
		opts := strings.Join(options, ",")
		if opts == "" {
			opts = "defaults"
		}
		l := []string{"lxc.mount.entry", fmt.Sprintf("%s %s %s %s 0 0", source, p, "none", opts)}
		configLines = append(configLines, l)
		return configLines, nil
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("Bad device type")
	}
}

func dbDeviceTypeToString(t int) (string, error) {
	switch t {
	case 0:
		return "none", nil
	case 1:
		return "nic", nil
	case 2:
		return "disk", nil
	case 3:
		return "unix-char", nil
	case 4:
		return "unix-block", nil
	default:
		return "", fmt.Errorf("Invalid device type %d", t)
	}
}

func deviceTypeToDbType(t string) (int, error) {
	switch t {
	case "none":
		return 0, nil
	case "nic":
		return 1, nil
	case "disk":
		return 2, nil
	case "unix-char":
		return 3, nil
	case "unix-block":
		return 4, nil
	default:
		return -1, fmt.Errorf("Invalid device type %s", t)
	}
}

func validDeviceConfig(t, k, v string) bool {
	if k == "type" {
		return false
	}
	switch t {
	case "unix-char":
		switch k {
		case "path":
			return true
		case "major":
			return true
		case "minor":
			return true
		case "uid":
			return true
		case "gid":
			return true
		case "mode":
			return true
		default:
			return false
		}
	case "unix-block":
		switch k {
		case "path":
			return true
		case "major":
			return true
		case "minor":
			return true
		case "uid":
			return true
		case "gid":
			return true
		case "mode":
			return true
		default:
			return false
		}
	case "nic":
		switch k {
		case "parent":
			return true
		case "name":
			return true
		case "hwaddr":
			return true
		case "mtu":
			return true
		case "nictype":
			return shared.StringInSlice(v, []string{"physical", "bridged", "p2p", "macvlan"})
		default:
			return false
		}
	case "disk":
		switch k {
		case "path":
			return true
		case "source":
			return true
		case "readonly", "optional":
			return true
		default:
			return false
		}
	case "none":
		return false
	default:
		return false
	}
}

func tempNic() string {
	randBytes := make([]byte, 4)
	rand.Read(randBytes)
	return "veth" + hex.EncodeToString(randBytes)
}

func inList(l []string, s string) bool {
	for _, ls := range l {
		if ls == s {
			return true
		}
	}
	return false
}

func nextUnusedNic(c container) string {
	lxContainer := c.LXContainerGet()

	list, err := lxContainer.Interfaces()
	if err != nil || len(list) == 0 {
		return "eth0"
	}
	i := 0
	// is it worth sorting list?
	for {
		nic := fmt.Sprintf("eth%d", i)
		if !inList(list, nic) {
			return nic
		}
		i = i + 1
	}
}

func setupNic(tx *sql.Tx, c container, name string, d map[string]string) (string, error) {
	var err error

	// A few checks
	if d["nictype"] == "" {
		return "", fmt.Errorf("Missing nic type")
	}

	if !shared.StringInSlice(d["nictype"], []string{"bridged", "physical", "p2p", "macvlan"}) {
		return "", fmt.Errorf("Bad nic type: %s", d["nictype"])
	}

	if shared.StringInSlice(d["nictype"], []string{"bridged", "physical", "macvlan"}) && d["parent"] == "" {
		return "", fmt.Errorf("Missing parent for %s type nic.", d["nictype"])
	}

	// Fill missing fields
	if d["name"] == "" {
		d["name"] = nextUnusedNic(c)
	}

	// Generate MAC if needed
	key := fmt.Sprintf("volatile.%s.hwaddr", name)
	config := c.ExpandedConfig()
	hwaddr := config[key]

	if hwaddr == "" {
		if d["hwaddr"] != "" {
			hwaddr, err = generateMacAddr(d["hwaddr"])
			if err != nil {
				return "", err
			}
		} else {
			hwaddr, err = generateMacAddr("00:16:3e:xx:xx:xx")
			if err != nil {
				return "", err
			}
		}

		if hwaddr != d["hwaddr"] {
			stmt := `INSERT OR REPLACE into containers_config (container_id, key, value) VALUES (?, ?, ?)`
			_, err = tx.Exec(stmt, c.ID(), key, hwaddr)

			if err != nil {
				return "", err
			}
		}
	}

	// Create the device
	var dev string

	if shared.StringInSlice(d["nictype"], []string{"bridged", "p2p"}) {
		n1 := tempNic()
		n2 := tempNic()

		err := exec.Command("ip", "link", "add", n1, "type", "veth", "peer", "name", n2).Run()
		if err != nil {
			return "", err
		}

		if d["nictype"] == "bridge" {
			err = exec.Command("brctl", "addif", d["parent"], n1).Run()
			if err != nil {
				removeInterface(n2)
				return "", err
			}
		}

		dev = n2
	}

	if d["nictype"] == "physical" {
		dev = d["parent"]
	}

	if d["nictype"] == "macvlan" {
		n1 := tempNic()

		err := exec.Command("ip", "link", "add", n1, "type", "macvlan", "link", d["parent"], "mode", "bridge").Run()
		if err != nil {
			return "", err
		}

		dev = n1
	}

	// Set the MAC address
	err = exec.Command("ip", "link", "set", "dev", dev, "address", hwaddr).Run()
	if err != nil {
		removeInterface(dev)
		return "", err
	}

	// Bring the interface up
	err = exec.Command("ip", "link", "set", "dev", dev, "up").Run()
	if err != nil {
		removeInterface(dev)
		return "", err
	}

	return dev, nil
}

func removeInterface(nic string) {
	_ = exec.Command("ip", "link", "del", nic).Run()
}

/*
 * Detach an interface in a container
 * The thing is, there doesn't seem to be a good way of doing
 * this without relying on /sys in the container or /sbin/ip
 * in the container being reliable.  We can look at the
 * /sys/devices/virtual/net/$name/ifindex (i.e. if 7, then delete 8 on host)
 * we can just ip link del $name in the container.
 *
 * if we just did a lxc config device add of this nic, then
 * lxc simply doesn't know the peername for this nic
 *
 * probably the thing to do is re-exec ourselves asking to
 * setns into the container's netns (only) and remove the nic.  for
 * now just don't do it, but don't fail either.
 */
func detachInterface(c container, key string) error {
	options := lxc.DefaultAttachOptions
	options.ClearEnv = false
	options.Namespaces = syscall.CLONE_NEWNET
	nullDev, err := os.OpenFile(os.DevNull, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer nullDev.Close()
	nullfd := nullDev.Fd()
	options.StdinFd = nullfd
	options.StdoutFd = nullfd
	options.StderrFd = nullfd
	command := []string{"ip", "link", "del", key}
	lxContainer := c.LXContainerGet()
	_, err = lxContainer.RunCommand(command, options)
	return err
}

func txUpdateNic(tx *sql.Tx, cId int, devname string, nicname string) error {
	q := `
	SELECT id FROM containers_devices
	WHERE container_id == ? AND type == 1 AND name == ?`
	var dId int
	err := tx.QueryRow(q, cId, devname).Scan(&dId)
	if err != nil {
		return err
	}

	stmt := `INSERT OR REPLACE into containers_devices_config (container_device_id, key, value) VALUES (?, ?, ?)`
	_, err = tx.Exec(stmt, dId, "name", nicname)
	return err
}

func (c *containerLXD) DetachUnixDev(dev shared.Device) error {
	cginfo, err := deviceCgroupInfo(dev)
	if err != nil {
		return err
	}
	c.c.SetCgroupItem("devices.remove", cginfo)
	pid := c.c.InitPid()
	if pid == -1 { // container not running
		return nil
	}
	pidstr := fmt.Sprintf("%d", pid)
	if err := exec.Command(os.Args[0], "forkumount", pidstr, dev["path"]).Run(); err != nil {
		shared.Log.Warn("Error unmounting device", log.Ctx{"Error": err})
		return err
	}
	if err := os.Remove(fmt.Sprintf("/proc/%d/root/%s", pid, dev["path"])); err != nil {
		shared.Log.Warn("Error removing device", log.Ctx{"Error": err})
		return err
	}

	return nil
}

func (c *containerLXD) AttachUnixDev(dev shared.Device) error {
	return c.setupUnixDev(dev)
}

/*
 * Given a running container and a list of devices before and after a
 * config change, update the devices in the container.
 *
 * Currently we only support nics.  Disks will be supported once we
 * decide how best to insert them.
 */
func devicesApplyDeltaLive(tx *sql.Tx, c container, preDevList shared.Devices, postDevList shared.Devices) error {
	rmList, addList := preDevList.Update(postDevList)
	var err error

	for key, dev := range rmList {
		switch dev["type"] {
		case "nic":
			if dev["name"] == "" {
				return fmt.Errorf("Do not know a name for the nic for device %s", key)
			}
			if err := detachInterface(c, dev["name"]); err != nil {
				return fmt.Errorf("Error removing device %s (nic %s) from container %s: %s", key, dev["name"], c.Name(), err)
			}
		case "disk":
			err = c.DetachMount(dev)
			if err != nil {
				return err
			}
		case "unix-block":
			err = c.DetachUnixDev(dev)
			if err != nil {
				return err
			}
		case "unix-char":
			err = c.DetachUnixDev(dev)
			if err != nil {
				return err
			}
		}
	}

	lxContainer := c.LXContainerGet()

	for key, dev := range addList {
		switch dev["type"] {
		case "nic":
			var tmpName string
			if tmpName, err = setupNic(tx, c, key, dev); err != nil {
				return fmt.Errorf("Unable to create nic %s for container %s: %s", dev["name"], c.Name(), err)
			}
			if err := lxContainer.AttachInterface(tmpName, dev["name"]); err != nil {
				removeInterface(tmpName)
				return fmt.Errorf("Unable to move nic %s into container %s as %s: %s", tmpName, c.Name(), dev["name"], err)
			}

			if err := txUpdateNic(tx, c.ID(), key, dev["name"]); err != nil {
				shared.Debugf("Warning: failed to update database entry for new nic %s: %s", key, err)
				return err
			}
		case "disk":
			if dev["source"] == "" || dev["path"] == "" {
				return fmt.Errorf("no source or destination given")
			}
			err = c.AttachMount(dev)
			if err != nil {
				return err
			}
		case "unix-block":
			err = c.AttachUnixDev(dev)
			if err != nil {
				return err
			}
		case "unix-char":
			err = c.AttachUnixDev(dev)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
