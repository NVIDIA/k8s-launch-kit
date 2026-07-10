This quick start guide covers five essential networking configurations for
different computational requirements. See :doc:`maintenance` when sizing node
disruption and upgrade concurrency for larger clusters.

Discovery and profile resolution
--------------------------------

``l8k discover`` writes both the discovered hardware inventory and the final
profile settings to ``cluster-config.yaml``. Missing profile fields are filled
from hardware and built-in defaults, existing values from ``--user-config``
are preserved, and explicit CLI flags take precedence over both.

.. code-block:: bash

   l8k discover --kubeconfig ~/.kube/config \
     --save-cluster-config ./cluster-config.yaml

   # The saved profile is sufficient; flags are optional overrides here.
   l8k generate --user-config ./cluster-config.yaml \
     --save-deployment-files ./deployment

When ``generate`` uses a file-backed config, it writes resolved defaults and
explicit CLI overrides back to that same file before rendering manifests.
Comments in the original YAML are preserved.

Discovery defaults ``deployment`` to ``sriov`` and ``multirail`` to ``true``.
It derives ``fabric`` only when every discovered group reports the same
confirmed link type. If the fabric cannot be confirmed, discovery still saves
the config with an empty fabric so it can be supplied explicitly later.

All profile flags accepted by ``generate`` are also accepted by ``discover``:
``--fabric``, ``--deployment-type``, ``--multirail``, ``--routing``,
``--ignore-arp``, ``--spectrum-x``, ``--multiplane-mode``, and
``--number-of-planes``. Explicit false is supported for YAML
(``multirail: false``, ``ignoreARP: false``) and CLI
(``--multirail=false``, ``--ignore-arp=false``) and remains stable when
discovery rewrites the file.

For routed multi-rail IPv4/RoCE deployments, ``--routing source-based`` chains
the automatic ``sbr`` CNI meta-plugin on generated non-Spectrum-X secondary
networks so traffic sourced from a rail IP exits through that rail's interface
and gateway. ``--ignore-arp`` chains the ``tuning`` CNI meta-plugin before
``sbr`` and sets ``arp_ignore=1``, ``arp_announce=2``, and ``rp_filter=0`` at
both ``all`` and ``IFNAME`` scopes. This is useful when pod rails can observe
ARP for each other: Linux can otherwise answer ARP for a rail-0 IP from a
rail-3 VF MAC inside the same network namespace, sending RoCE traffic to the
wrong HCA even though the IP destination is correct. These settings apply to
SR-IOV, SR-IOV IB, host-device, Macvlan RDMA-shared, and IPoIB RDMA-shared
profiles; they do not apply to Spectrum-X profiles.

.. code-block:: bash

   l8k discover --user-config ./cluster-config.yaml \
     --kubeconfig ~/.kube/config \
     --fabric infiniband --deployment-type rdma_shared \
     --multirail=false

.. toctree::
   :hidden:
   :maxdepth: 1
   :caption: Quick Start Guide

   SR-IOV Network with RDMA <sriov-network-rdma>
   Host Device Network with RDMA <host-device-rdma>
   IP over InfiniBand with RDMA Shared Device <ipoib-rdma-shared>
   MacVLAN Network with RDMA Shared Device <macvlan-rdma-shared>
   SR-IOV InfiniBand Network with RDMA <sriov-ib-rdma>
   Maintenance and upgrade concurrency <maintenance>

.. list-table::
   :widths: 20 25 20 30
   :header-rows: 1

   * - **Use Case**
     - **Purpose**
     - **Performance Requirements**
     - **Applications**
   * - :doc:`SR-IOV Network with RDMA <sriov-network-rdma>`
     - High-performance networking with hardware acceleration
     - • >10 Gbps throughput
       • <1μs latency
       • Dedicated VF resources
     - HPC simulations, distributed ML training, financial trading
       
       *Keywords: SR-IOV, RDMA, HPC, low-latency, VF isolation*
   * - :doc:`Host Device Network with RDMA <host-device-rdma>`
     - Direct hardware access for legacy applications
     - • Raw device control
       • Exclusive hardware access
       • Minimal CPU overhead
     - Legacy HPC codes, specialized protocols, DPDK applications
       
       *Keywords: host-device, PCI-passthrough, direct-access, exclusive-access*
   * - :doc:`IP over InfiniBand with RDMA Shared Device <ipoib-rdma-shared>`
     - InfiniBand networking with shared RDMA resources
     - • >50 Gbps bandwidth
       • Parallel I/O workloads
       • Shared device efficiency
     - Distributed storage, data analytics, scientific computing
       
       *Keywords: InfiniBand, IPoIB, shared-device, high-bandwidth*
   * - :doc:`MacVLAN Network with RDMA Shared Device <macvlan-rdma-shared>`
     - Network isolation with shared RDMA capabilities
     - • Multi-tenant segmentation
       • 10+ pods per node
       • Moderate throughput
     - Cloud-native HPC, microservices, multi-tenant ML
       
       *Keywords: MacVLAN, multi-tenant, network-segmentation, resource-sharing*
   * - :doc:`SR-IOV InfiniBand Network with RDMA <sriov-ib-rdma>`
     - Virtualized InfiniBand with hardware acceleration
     - • >100 Gbps bandwidth
       • Hardware acceleration
       • Isolated IB partitions
     - Large-scale HPC clusters, AI/ML training, research computing
       
       *Keywords: SR-IOV, InfiniBand, hardware-acceleration, ultra-high-bandwidth*

.. seealso::

   Large clusters can tune simultaneous SR-IOV configuration and DOCA/OFED
   upgrades through the ``maintenance`` section. See :doc:`maintenance` for
   the defaults, release gates, and disruption-budget restrictions.
