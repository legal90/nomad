---
layout: "docs"
page_title: "Drivers: VZ"
sidebar_current: "docs-drivers-vz"
description: |-
  The VZ task driver is used to run Linux containers on Virtuozzo and OpenVZ.
---

# VZ Driver

Name: `vz`

The `VZ` driver provides an interface for running containers (CT) on Virtuozzo
and OpenVZ. Currently, the driver supports launching containers but does not
support dynamic ports. This driver is being marked as experimental and should
be used with care.

## Task Configuration

The `VZ` driver supports the following configuration in the job spec:

* `os_template` - Name of OS template which should be used to create a new CT.

* `config_name` - (Optional) If this option is specified, values from example
  configuration file `/etc/vz/conf/ve-<NAME>.conf-sample` will be put into CT
  configuration file. If CT configuration file already exists, it will be
  overridden.

* `private_path` - (Optional) Path to directory in which all the files and
  directories specific to this very CT are stored. Default is `VE_PRIVATE` value
  specified in `/etc/vz/vz.conf`. Option can contain literal string `$VEID`,
  which will be substituted with numeric CT ID.

* `root_path` - (Optional) Path to the mount point for CT root directory.
  Default is `VE_ROOT` value specified in `/etc/vz/vz.conf`. Option can contain
  literal string `$VEID`, which will be substituted with numeric CT ID.

* `hostname` - (Optional) The hostname to assign to the container. When
  launching more than one of a task (using `count`) with this option set, every
  container the task starts will have the same hostname.

* `dns_servers` - (Optional) A list of DNS servers for the container to use
  (e.g. `["8.8.8.8", "8.8.4.4"]`).

* `dns_search_domains` - (Optional) A list of DNS search domains for the container
  to use.

* `network` - (Optional) `network` is a repeatable object that can be used to
  configure network adapters for CT. The format is described below.

### Networking

The name of `network` object determines the network interface name in the CT.
The object itself supports the following keys:

* `ip` - A list of IPv4 addresses to be assigned to the interface. Network mask
could be appended with `/`, for example: `["192.168.33.33/24", 10.211.55.18]`

* `gateway` - (Optional) Default IPv4 gateway for the network interface

* `network_name` - (Optional) Name (e.q. ID) of virtual network to connect this
   adapter to. *For Virtuozzo only!*

Example:

```
config {
  // ...
  
  network "eth0" {
    ip = ["10.123.44.237/23","10.123.44.76/24"]
    gateway = "10.123.44.1"
    network_name = "DEV"
  }

  network "eth1" {
    ip = ["192.168.10.33/255.255.255.0"]
    gateway = "192.168.10.1"
    network_name = "PUB"
  }
}
```

## Client Requirements

The `VZ` driver requires OpenVZ 4+ or Virtuozzo 6+ to be installed on the node.
Utilities `vzctl` and `vzlist` should be available the system's `$PATH`.
The `os_template` must be pre-installed on the node. For OpenVZ it expects that
an appropriate .tar.gz archive with is available in `/vz/template/cache`.
For Virtuozzo it could be also done by installing EZ template as RPM package.

## Client Attributes

The `VZ` driver will set the following client attributes:

* `driver.vz` - Set to `1` if any of OpenVZ or Virtuozzo is found on the host
node. Nomad determines this by executing `vzctl --version` on the host and
parsing the output
* `driver.qemu.version` - Version of `vzctl`, ex: `4.9.4` or `6.0.8`

## Resource Isolation

Nomad uses Virtuozzo/OpenVZ to provide operating-system-level virtualization for
Linux container workloads. VZ containers are isolated entities which perform and
executes exactly like a stand-alone servers.
