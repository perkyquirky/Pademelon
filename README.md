# Pademelon

## What is Pademelon?

I am developing Pademelon to enhance the functionality of Truenas Scales libvirt/kvm virtual machine experience. Currently the Truenas functionality leaves a lot to be desired.

As someone who just wants to run no more than 5 VM's I like the simplicity of just using what is there, rather than virtualising Truenas in something like Proxmox. But that doesn't mean the Truenas virtualisation experience needs to be bad, especially when we can take advantage of QEMU guest agents.

## What is a Pademelon?

A Pademelon is similar to a Kangaroo or Wallaby. It is a marsupial, native to Australia. They are generally small in size, similar to a Quokka, though Pademelons are a group of macropods, where as a Quokka is a sole member of it's genus. 

## Why the name Pademelon?

Marsupials carry their young in a pouch, which makes me think of a hypervisor with a VM running inside. 

Also they are adorable.

## Currently supported:

- VM power status
- QEMU guest agent detection
- Hostname
- CPU core count
- CPU usage
- Memory usage
- IP address & interface ID
- Host OS & kernel (linux)
- Drive partitions & usage

## Currently unsupported:

- Windows VM's
- Any write operations e.g:
  - Stop / restart
  - Any kind of writing to the VM

## Docker Compose

There is a compose file in the root directory of this repository for you to copy.

You can adjust settings in the compose file with the following:

``` yaml

# DO NOT USE THIS AS A COMPOSE FILE.
# IT IS FOR DEMONSTRATION AND HAS CONFLICTING INFORMATION.
# USE THE COMPOSE FILE IN THE REPOSITORY.

services:
  pademelon:
    image: ghcr.io/perkyquirky/pademelon:latest
    container_name: pademelon
    restart: unless-stopped

    ports:
      - "8088:8088"

    volumes:
      - /run/truenas_libvirt:/run/truenas_libvirt
    user: "0:0"

    command:
    # -listen changes the internal listen port, dont change unless you know why you would
      - "-listen=:8088"

    # Sets how many VM's to poll at once. If you have more than 8, increase this number
      - "-concurrency=8"

    # This sets the polling rate for all stats. As the data needs to be queried to the libvirt socket manually to get fresh data this needs to be set.
    # CPU usage, memory usage, network info, drive data, agent status, etc. are all controlled with this timer. 
      - "-interval=30s"

    # As a single timer is used to poll for data from the VM, if one stat hangs, for example hard drive data is not returned, 
    # the time out is set to 5 seconds to ensure the other data is successfully returned.
      - "-agent-timeout=5s"

    # This is required to get fresh memory statistics, without it stale data is returned. Do not adjust unless necessary
      - "-stats-period=10s"
    
    # Sets the level of logging: `info`, `debug`, `info`, `warn`, `error` 
      - "-log-level=info"

    # Sets the logging output format:  `text`, `text`, `json`
      - "-log-format=text"

    # Sets the path for the libvirt socket. This wont need changing unless truenas change the location of the socket.
    # If this changes you need to also need to change the volumes path to the socket too.
      - "-socket=/run/truenas_libvirt"
    # For example if the path needs changing you need to change both the socket and mount:
      volumes:
        - /var/run/libvirt:/var/run/libvirt

        - "-socket=/var/run/libvirt"
    # One convention note: older libvirt setups often use /var/run/libvirt/libvirt-sock but on TrueNAS
    # Truenas uses a symlink to /run/... — mount the real /run/... path, since /var/run may not resolve inside this scratch-based container.


```