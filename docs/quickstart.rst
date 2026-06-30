This quick start guide covers five essential networking configurations for
different computational requirements. See :doc:`maintenance` when sizing node
disruption and upgrade concurrency for larger clusters.

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
