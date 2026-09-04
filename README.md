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
- Disk & network throughput
- Agent version & clock drift per VM
- VM raw XML viewer
- Manual refresh button
- Colour themes — nine built in, pick one in the page header or set the server default with `-theme`
- Optional token auth — see "Optional auth" below

## How to use?

Pademelon requires that you have the QEMU Guest Agent installed in your VM's. 

You do not have to change any settings in Truenas to enable the guest agent functionality. Just install the agent in your VM's and you are good to go.

The [Proxmox documentation](https://pve.proxmox.com/wiki/Qemu-guest-agent) has the easiest to understand guide on how to install the guest agent.


## Docker Compose

There is a compose file in the root directory of this repository for you to copy.

[Compose File](https://github.com/perkyquirky/Pademelon/blob/main/docker-compose.yaml)

More example configurations, including auth: [Compose Examples](https://github.com/perkyquirky/Pademelon/blob/main/docker-compose-examples.yaml)

## Optional authentication

By default the dashboard is open to anyone who can reach it on the local network.

Users can view live stats without authenticating. 

Pademelon will soon have features for controlling VM states. As such Auth is being implimented to ensure that people on the local network cannot make changes unauthenticated. The auth system is NOT intended AT ALL for internet facing.

You can use a password of your choosing or generate a random string with:

```bash
openssl rand -hex 32
```

Then pass that value to the container as the `PADAMELON_TOKEN` environment variable, or via a Docker secret file with `PADAMELON_TOKEN_FILE`. A "log in" button appears in the page header; enter the token once per browser and you stay logged in for 30 days, with a "log out" button to clear the session.

## Use via compose example: auth

```yaml
services:
  pademelon:
    image: ghcr.io/perkyquirky/pademelon:latest
    container_name: pademelon
    restart: unless-stopped
    ports:
      - "8088:8088"
    environment:
      # Generate with: openssl rand -hex 32 or use a password of your choice. 
      - PADAMELON_TOKEN=change-me-to-your-generated-token
    volumes:
      - /run/truenas_libvirt:/run/truenas_libvirt
    user: "0:0"
```

Prefer not to have the token in your compose file? Use the Docker-secrets convention instead: `PADAMELON_TOKEN_FILE=/run/secrets/pademelon_token` with the secret mounted at that path.

