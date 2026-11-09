# `microovn setup bgp-attach`

Attach mode currently needs six markers to be set: two in every chassis OVS
and four in OVN NB.

On each node:

```bash
sudo ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp=enabled
sudo ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-advertise-table=42
```

Cluster wide:

```bash
LRP=bgp-lrp-bgp-lr-stg-reproducer-hassahma-compute-0-to-bgp-ls-stg-reproducer-hassahma-compute-0-br-bgp

sudo ovn-nbctl set logical_router_port $LRP \
  networks="192.0.2.3/31" \
  options:dynamic-routing-maintain-vrf=true \
  external-ids:microovn-bgp-attach=true \
  external-ids:microovn-bgp-peers="65101@192.0.2.2"
```

This PR adds one command that configures it per leaf (rack), so Sunbeam can
drive it:

```bash
microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --members c0,c1,c2
```

Here, `192.0.2.18/28` is the range for assigning IPs to each compute node c0,
c1 and c2, starting from `192.0.2.18`.

In the above example:

```
192.0.2.18 -> c0
192.0.2.19 -> c1
192.0.2.20 -> c2
```

MicroOVN owns the lifecycle of creating the veth interface on each node and
assigning an IP address to it:

```
mp977ed87fo@mp977ed87fb  UP  ...
mp977ed87fb@mp977ed87fo  UP  192.0.2.18
```

## Different BGP peers for different computes

Run the command once per peer (leaf). Each `--peer` is a separate setting with
its own `--networks` and `--members`; running it for one peer does not change
the others.

```bash
# rack1 computes peer with leaf 1
microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --members c0,c1,c2

# rack2 computes peer with leaf 2
microovn setup bgp-attach --peer 65102@192.0.2.33 --networks 192.0.2.34/28 --members c3,c4
```

- `--members` is the complete list for that peer: rerunning a peer with a
  different list adds/removes computes for that peer only.
- A compute can belong to one peer only. To move c2 to leaf 2, first rerun
  leaf 1 without it, then rerun leaf 2 with it.
- Each peer needs its own subnet; overlapping `--networks` or a shared leaf
  address are refused.
- `microovn setup bgp-attach status` lists every peer and its computes;
  `microovn setup bgp-attach disable --peer <peer>` removes one peer's setting.
