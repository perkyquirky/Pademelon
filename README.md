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
