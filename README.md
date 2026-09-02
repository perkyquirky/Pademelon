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

Windows and Linux (tested Windows Server 2022 & Ubuntu Server 24.04.4 LTS)

- VM power status
- QEMU guest agent detection
- Hostname
- CPU core count
- CPU usage
- Memory usage
- IP address & interface ID
- Host OS - kernel (Linux) / OS build (Windows)
- Drive partitions & usage
- Colour themes — nine built in, pick one in the page header or set the server default with `-theme`

## Currently unsupported:

- Any write operations e.g:
  - Stop / restart
  - Any kind of writing to the VM

## How to use?

Pademelon requires that you have the QEMU Guest Agent installed in your VM's. 

You do not have to change any settings in Truenas to enable the guest agent functionality. Just install the agent in your VM's and you are good to go.

The [Proxmox documentation](https://pve.proxmox.com/wiki/Qemu-guest-agent) has the easiest to understand guide on how to install the guest agent.


## Docker Compose

There is a compose file in the root directory of this repository for you to copy.

[Compose File](https://github.com/perkyquirky/Pademelon/blob/main/docker-compose.yaml)

