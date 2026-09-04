# TrueNAS Shell Commands

My list of probes for poking at VMs from the TrueNAS shell. 

## The connection string

TrueNAS puts the libvirt socket somewhere non-standard, so every `virsh` call needs the full connection string. The single quotes are required - the URI contains `?` and `=`, and the shell mangles it without them:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock'
```

Everything below is that prefix plus a subcommand. Guest agent commands need the full socket: the read-only one that also sits in that directory (`libvirt-sock-ro`) answers most queries but refuses agent commands with `operation forbidden: read only prevents virDomainQemuGuestAgentCommand`.

## First things to run

What VMs exist:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' list --all
```

Is the agent channel up? `state='connected'`; `disconnected` means the agent isn't running inside the guest.

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' dumpxml 12_test | grep -A2 guest_agent
```

## Is the agent alive?

`guest-ping` - a healthy agent answers with an empty object. `--timeout 5` makes a wedged agent error out after 5 seconds instead of hanging the shell forever:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-ping"}'
```

```shell
{"return":{}}
```

`guest-info` lists every command this guest's agent supports. Note: older agents and Windows builds differ:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-info"}'
```

### File system

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-fsinfo"}'
```

```shell
{"return":[{"name":"\\\\?\\Volume{e22d3098-a70b-11f1-abc2-806e6f6e6963}\\","total-bytes":877373440,"mountpoint":"E:\\","disk":[{"bus-type":"sata","bus":0,"unit":0,"pci-controller":{"bus":-1,"slot":-1,"domain":-1,"function":-1},"dev":"\\\\?\\Volume{e22d3098-a70b-11f1-abc2-806e6f6e6963}","target":0}],"used-bytes":877373440,"type":"CDFS"},{"name":"\\\\?\\Volume{e22d3097-a70b-11f1-abc2-806e6f6e6963}\\","total-bytes":6559221760,"mountpoint":"D:\\","disk":[{"bus-type":"sata","bus":0,"unit":0,"pci-controller":{"bus":-1,"slot":-1,"domain":-1,"function":-1},"dev":"\\\\?\\Volume{e22d3097-a70b-11f1-abc2-806e6f6e6963}","target":0}],"used-bytes":6559221760,"type":"UDF"},{"name":"\\\\?\\Volume{279e790b-c8f7-433a-88f2-3f375b5536a1}\\","total-bytes":100663296,"mountpoint":"System Reserved","disk":[{"serial":"zqKZeDEF","bus-type":"scsi","bus":0,"unit":0,"pci-controller":{"bus":4,"slot":0,"domain":0,"function":0},"dev":"\\\\.\\PhysicalDrive0","target":0}],"used-bytes":31732736,"type":"FAT32"},{"name":"\\\\?\\Volume{ab0b9835-9214-4fce-b7ce-7697a78d8cc4}\\","total-bytes":870313984,"mountpoint":"System Reserved","disk":[{"serial":"zqKZeDEF","bus-type":"scsi","bus":0,"unit":0,"pci-controller":{"bus":4,"slot":0,"domain":0,"function":0},"dev":"\\\\.\\PhysicalDrive0","target":0}],"used-bytes":750354432,"type":"NTFS"},{"name":"\\\\?\\Volume{e3f499ea-ed8f-4391-9ec7-d0a08fced49}\\","total-bytes":52691988480,"mountpoint":"C:\\","disk":[{"serial":"zqKZeDEF","bus-type":"scsi","bus":0,"unit":0,"pci-controller":{"bus":4,"slot":0,"domain":0,"function":0},"dev":"\\\\.\\PhysicalDrive0","target":0}],"used-bytes":14206033920,"type":"NTFS"}]}
```

### OS Info

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-osinfo"}'
```

```shell
{"return":{"name":"Microsoft Windows","kernel-release":"20348","version":"Microsoft Windows Server 2022","variant":"server","pretty-name":"Windows Server 2022 Standard","version-id":"2022","variant-id":"server","kernel-version":"10.0","machine":"x86_64","id":"mswindows"}}
```

### Network info

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-network-get-interfaces"}'
```

```shell
{"return":[{"name":"Ethernet","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"fe80::3a11:dfb6:9b8f:8241%6","prefix":64},{"ip-address-type":"ipv4","ip-address":"192.168.1.81","prefix":24}],"statistics":{"tx-packets":7859,"tx-errs":0,"rx-bytes":18569388,"rx-dropped":2862,"rx-packets":15630,"rx-errs":0,"tx-bytes":1919654,"tx-dropped":0},"hardware-address":"00:a0:98:63:a7:d1"},{"name":"Loopback Pseudo-Interface 1","ip-addresses":[{"ip-address-type":"ipv6","ip-address":"::1","prefix":128},{"ip-address-type":"ipv4","ip-address":"127.0.0.1","prefix":8}],"statistics":{"tx-packets":0,"tx-errs":0,"rx-bytes":0,"rx-dropped":0,"rx-packets":0,"rx-errs":0,"tx-bytes":0,"tx-dropped":0}}]}
```

### More agent commands

All the same shape - swap the command in the JSON:

```shell
# hostname the guest reports
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-host-name"}'
# guest clock — suspect this when timestamps or TLS look insane in the guest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-time"}'
# timezone
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-timezone"}'
# vCPU count as the guest sees it
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-vcpus"}'
# who is logged in
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-users"}'
# disks/block devices the guest sees (no filesystems — pair with get-fsinfo)
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-disks"}'
# PCI devices and which driver the guest loaded on each (virtio debugging on Windows)
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-devices"}'
# memory banks / DIMM layout as the guest sees it
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-memory-blocks"}'
```

## One-shot aggregators

Collect lots of data:

```shell
# everything the agent has reported, in one dump
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' guestinfo 13_wintest --pretty
# hostname, via the agent
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domhostname 13_wintest
# IP addresses, via the agent — same data as guest-network-get-interfaces, less JSON
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domifaddr 13_wintest --source agent
```

`domifaddr --source lease` reads DHCP leases from the libvirt network instead - useful when the agent is dead but the VM sits on the default NAT network.

## Host side (no agent needed)

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' dominfo 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domstate 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domstats 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domblklist 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' domiflist 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' cpu-stats 13_wintest
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' nodememstats
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' version --daemon
```

`domstats` is the everything-at-once one — one line per subsystem (CPU, vCPU, balloon, block, net). Narrow it with flags: `--cpu-total`, `--balloon`, `--vcpu`, `--block`, `--net`.

### Memory

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' dommemstat 13_wintest
```

```shell
actual 12582912
last_update 1788317348
rss 12675976
```

Is the balloon stats poll timer set? Without `<stats period='...'/>` QEMU collects one snapshot per boot and the numbers never move.

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' dumpxml 13_wintest | grep -iA2 memballoon
```

```shell
    <memballoon model='virtio'>
      <stats period='10'/>
      <alias name='balloon0'/>
--
    </memballoon>
  </devices>
  <seclabel type='dynamic' model='apparmor' relabel='yes'>

```

## QEMU monitor

The QEMU monitor can also start, stop and mangle VMs, so stick to these exact lines and nothing else:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-monitor-command 13_wintest --hmp 'info balloon'
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-monitor-command 13_wintest --hmp 'info block'
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-monitor-command 13_wintest --hmp 'info status'
```

## Probe every VM at once

Ping every running VM's agent — prints `ok`/`FAIL` per VM. This is the fastest way to find the one wedged guest agent stalling Pademelon's poll:

```shell
for d in $(virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' list --state-running --name); do virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command "$d" --timeout 5 '{"execute":"guest-ping"}' >/dev/null 2>&1 && echo "$d ok" || echo "$d FAIL"; done
```

Agent channel state for every VM, including stopped ones:

```shell
for d in $(virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' list --all --name); do printf '%s: ' "$d"; virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' dumpxml "$d" | grep -A2 guest_agent | grep -oE "state='[a-z]+'" || echo "no channel"; done
```

## TrueNAS side

```shell
# both sockets and their permissions (libvirt-sock vs libvirt-sock-ro)
ls -la /run/truenas_libvirt/
# the live QEMU processes, with their full command lines (vCPUs, RAM, disks)
ps aux | grep qemu-system
# what TrueNAS middleware thinks the VMs are — output shape varies by version
midclt call vm.query | python3 -m json.tool
# find the libvirt service name, then read its last 100 log lines
systemctl list-units | grep -i virt
journalctl -u libvirtd -n 100 --no-pager
# libvirt + QEMU versions — check before assuming an agent command exists
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' version --daemon
# what Pademelon itself sees, when the container is running
curl -s http://localhost:8088/api/vms | python3 -m json.tool
```

TrueNAS has `python3` so `python3 -m json.tool` is good for pretty-printing any of the JSON above:

```shell
virsh -c 'qemu+unix:///system?socket=/run/truenas_libvirt/libvirt-sock' qemu-agent-command 13_wintest --timeout 5 '{"execute":"guest-get-fsinfo"}' | python3 -m json.tool
```
