Predefined Cluster Configuration Presets
========================================

Overview
--------

l8k supports **predefined topology presets** for known server machine types. Presets provide authoritative hardware topology data that replaces heuristic-based discovery results, including:

- **Traffic classification** (east-west vs north-south) for each NIC port
- **Rail assignments** for east-west ports
- **NUMA node affinity** for each NIC port
- **GPU proximity** (which GPU each NIC is physically closest to)

When a cluster node's machine type matches a preset and its PCI topology validates against the discovered hardware, the preset is automatically applied during discovery.

How It Works
------------

During ``l8k discover``, after determining a node group's machine type (from GPU operator labels or DMI data), l8k checks the local presets directory for a matching entry:

1. **Lookup**: Searches for ``<presets-dir>/<machine-type>/topology.yaml``
2. **Validation**: Compares PCI addresses and device IDs between the preset and discovered NicDevices
3. **Application**: If validation passes, overrides traffic classification, rail assignments, and adds NUMA/GPU topology metadata
4. **Fallback**: If no preset matches or validation fails, falls back to dynamic heuristic-based discovery

The preset directory is resolved using the same lookup chain as profiles:

- ``./presets`` (current working directory)
- ``/usr/local/share/l8k/presets`` (default install)
- ``<binary-dir>/../share/l8k/presets`` (custom prefix install)

Validation
----------

Preset validation ensures the preset matches the actual hardware present in the cluster. The following checks are performed:

.. list-table::
   :widths: 20 40 20
   :header-rows: 1

   * - Check
     - Description
     - On Mismatch
   * - PF count
     - Number of PFs in preset must match discovered count
     - Preset rejected
   * - PCI addresses
     - Every PCI address must match between preset and discovered hardware
     - Preset rejected
   * - Device IDs
     - Device ID at each PCI address must match
     - Preset rejected
   * - Part numbers
     - Part numbers may differ across vendor SKUs
     - Warning logged, discovered value used
   * - PSIDs
     - PSIDs may differ across firmware versions
     - Warning logged, discovered value used

Available Presets
-----------------

The following presets are bundled with l8k:

.. list-table::
   :widths: 30 20 20 15
   :header-rows: 1

   * - Machine Type
     - GPU Product
     - NIC Model
     - Rails
   * - PowerEdge-XE9680
     - NVIDIA H200
     - BlueField-3 SuperNIC (ConnectX-7)
     - 8
   * - ThinkSystem-SR680a-V3
     - NVIDIA H200
     - BlueField-3 VPI (ConnectX-7)
     - 8
   * - UCSC-885A-M8-H22
     - NVIDIA H200
     - BlueField-3 E-series SuperNIC (ConnectX-7)
     - 8

Managing Presets
----------------

List local presets
^^^^^^^^^^^^^^^^^^

.. code-block:: bash

   l8k preset list

Download latest presets
^^^^^^^^^^^^^^^^^^^^^^^

Download the latest presets from the GitHub repository:

.. code-block:: bash

   l8k preset update

By default, presets are downloaded from the ``main`` branch of ``nvidia/k8s-launch-kit``. You can customize this:

.. code-block:: bash

   # Custom repository and branch
   l8k preset update --repo myorg/k8s-launch-kit --branch develop

   # Custom destination directory
   l8k preset update --dir /opt/l8k/presets

For authenticated requests (to avoid GitHub API rate limits), set the ``GITHUB_TOKEN`` environment variable:

.. code-block:: bash

   export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx
   l8k preset update

Install script
^^^^^^^^^^^^^^

The install script (``scripts/install.sh``) automatically installs bundled presets and optionally downloads the latest from GitHub:

.. code-block:: bash

   # Default: installs bundled presets and downloads latest
   scripts/install.sh

   # Skip downloading latest presets (use bundled only)
   scripts/install.sh --skip-presets-update

Topology YAML Format
--------------------

Each preset is a ``topology.yaml`` file in a machine-type-named directory. The format is a superset of the ``clusterConfig.pfs`` schema with additional topology metadata:

.. code-block:: yaml

   machineType: PowerEdge-XE9680
   productType: NVIDIA-H200
   nicModel: BlueField-3 B3140H E-series HHHL SuperNIC (ConnectX-7)
   gpuInterconnect: NV18
   numaNodes: 2

   pfs:
     - deviceID: a2dc
       pciAddress: 0000:1a:00.0
       traffic: east-west
       rail: 0
       numaNode: 0             # NUMA node affinity
       connectedGPU: GPU0      # Physically closest GPU
       gpuProximity: PIX       # PCI proximity level
       psid: mt_0000001069
       partNumber: 0KK4NR
     - deviceID: a2dc
       pciAddress: 0000:9d:00.0
       traffic: north-south    # DPU/management NIC
       numaNode: 1
       connectedGPU: ""
       gpuProximity: NODE
       psid: mt_0000000884
       partNumber: 0HFWRM

Adding New Presets
------------------

To add a preset for a new machine type:

1. Run the topology collector on a representative node of that machine type
2. Create a directory named after the machine type (matching DMI ``product_name`` with spaces replaced by dashes)
3. Place the ``topology.yaml`` file in that directory
4. Submit a pull request to the repository

The machine type string must match exactly what ``/sys/class/dmi/id/product_name`` returns (after space-to-dash sanitization), or the ``nvidia.com/gpu.machine`` node label value.
