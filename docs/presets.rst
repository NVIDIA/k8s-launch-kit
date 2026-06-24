Predefined Cluster Configuration Presets
========================================

Overview
--------

l8k supports **predefined topology presets** for known server machine types. Presets provide authoritative hardware topology data that replaces heuristic-based discovery results, including:

- **Traffic classification** (east-west vs north-south) for each NIC port
- **Rail assignments** for east-west ports
- **NUMA node affinity** for each NIC port
- **GPU proximity** (which GPU each NIC is physically closest to)

Presets serve two distinct flows:

1. **Discovery overlay** — When ``l8k discover`` runs, the matched preset's authoritative topology fields override what discovery's heuristics inferred.
2. **Ahead-of-time generation (`--for`)** — ``l8k generate --for <preset-name>`` skips cluster discovery entirely and builds the ``clusterConfig`` directly from the preset's static description.

Lookup Model
------------

Presets are matched on the pair ``(machineType, gpuType)``. Both fields are **required** in every ``topology.yaml``; a preset that omits either is rejected at load time. Lookup is **exact-match** — there is no any-GPU fallback. A preset with ``machineType: PowerEdge-XE9680`` and ``gpuType: NVIDIA-H200`` matches only nodes whose discovered values are exactly that pair.

This makes multi-variant presets straightforward: the same physical chassis with different GPU SKUs gets different presets. Directory naming for variants is free-form (composite names like ``PowerEdge-XE9680-H200`` and ``PowerEdge-XE9680-B200`` are recommended), but the directory name is **only** used by ``--for`` and ``l8k preset list``. The matching keys live inside the YAML.

The presets directory is resolved using the same lookup chain as profiles:

- ``./presets`` (current working directory)
- ``/usr/local/share/l8k/presets`` (default install)
- ``<binary-dir>/../share/l8k/presets`` (custom prefix install)

How Discovery Uses Presets
--------------------------

During ``l8k discover``, after determining a node group's ``machineType`` and ``gpuType`` (from GPU operator labels or DMI/``nvidia-smi`` fallback), l8k:

1. **Looks up** a preset whose YAML declares the exact same ``(machineType, gpuType)`` pair.
2. **Validates** PF count, PCI addresses, and device IDs between the preset and discovered NicDevices.
3. **Applies** the preset's authoritative topology fields — **only when the hardware matches exactly** (no PF-count / PCI-address / device-ID deviation), preserving live state (RDMA device, network interface, PSID, part number).
4. **Falls back** to heuristic discovery when no preset matches *or* when the matched preset deviates from the discovered hardware. In the deviation case the discrepancies are still recorded under ``clusterConfig[*].presetDeviation`` and a warning is emitted on every config load, but the preset's topology is **not** overlaid — overlaying a preset onto a different PCI layout would corrupt the live-discovered traffic/rail classification.

Generating From a Preset (``--for``)
------------------------------------

When you have a known SKU and want to render manifests ahead of time — without ``kubectl`` access to the cluster — pass ``--for <preset-name>`` to ``l8k generate``:

.. code-block:: bash

   l8k generate --user-config cluster-config.yaml \
     --for ThinkSystem-SR680a-V3 \
     --node-selector "nvidia.com/gpu.product=NVIDIA-H200" \
     --fabric ethernet --deployment-type sriov \
     --save-deployment-files ./output

Behaviour:

- ``--for`` takes the **directory name** (one of the values shown by ``l8k preset list``).
- ``--node-selector`` is **required**: the synthesized clusterConfig has no live worker-node list, so the selector is the only way to identify which nodes the manifests should target at apply time.
- ``--for`` and ``--discover-cluster-config`` are mutually exclusive.
- The preset's topology becomes the **entire** ``clusterConfig`` section (replacing whatever was in the user config). The rest of the user config (``networkOperator``, ``podNamespace``, etc.) is preserved.
- Profile selection (``FindApplicableProfile``) reads the preset's declared ``capabilities`` block to match a profile — so a preset used with ``--for`` must declare it (see below).

The synthesized ``ClusterConfig`` group has its ``Identifier`` set to the preset directory name. This guarantees that two variants of the same machine type produce distinct ``NicNodePolicy`` names.

Validation (Discovery Path)
---------------------------

Preset validation during ``l8k discover`` ensures the preset matches the actual hardware present in the cluster. The following checks are performed:

.. list-table::
   :widths: 20 40 20
   :header-rows: 1

   * - Check
     - Description
     - On Mismatch
   * - PF count
     - Number of PFs in preset must match discovered count
     - Preset not applied; deviation recorded
   * - PCI addresses
     - Every PCI address must match between preset and discovered hardware
     - Preset not applied; deviation recorded
   * - Device IDs
     - Device ID at each PCI address must match
     - Preset not applied; deviation recorded
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

   * - Directory
     - Machine Type
     - GPU Type
     - NIC Model
   * - PowerEdge-XE9680
     - PowerEdge-XE9680
     - NVIDIA-H200
     - BlueField-3 SuperNIC (ConnectX-7)
   * - ThinkSystem-SR680a-V3
     - ThinkSystem-SR680a-V3
     - NVIDIA-H200
     - BlueField-3 VPI (ConnectX-7)
   * - UCSC-885A-M8-H22
     - UCSC-885A-M8-H22
     - NVIDIA-H200
     - BlueField-3 E-series SuperNIC (ConnectX-7)

Managing Presets
----------------

List local presets
^^^^^^^^^^^^^^^^^^

.. code-block:: bash

   l8k preset list

Output shows each preset's directory name plus the ``machineType`` / ``gpuType`` it matches:

.. code-block:: text

   Available presets (presets):
     NAME                    MACHINETYPE              GPUTYPE
     PowerEdge-XE9680        PowerEdge-XE9680         NVIDIA-H200
     ThinkSystem-SR680a-V3   ThinkSystem-SR680a-V3    NVIDIA-H200
     UCSC-885A-M8-H22        UCSC-885A-M8-H22         NVIDIA-H200

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

Each preset is a ``topology.yaml`` file in a directory whose name uniquely identifies the variant. The schema:

.. code-block:: yaml

   # Required matching keys (both must be non-empty)
   machineType: PowerEdge-XE9680
   gpuType: NVIDIA-H200

   # Optional metadata
   manufacturer: Dell
   nicModel: BlueField-3 B3140H E-series HHHL SuperNIC (ConnectX-7)
   gpuInterconnect: NV18
   numaNodes: 2

   # Optional: required when this preset is used via `l8k generate --for`.
   # Discovery-time ApplyPreset ignores this block.
   capabilities:
     nodes:
       sriov: true
       rdma: true
       ib: false

   pfs:
     - deviceID: a2dc
       pciAddress: 0000:1a:00.0
       rdmaDevice: rocep26s0f0
       networkInterface: eth2
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
       gpuProximity: NODE
       psid: mt_0000000884
       partNumber: 0HFWRM

The ``capabilities`` block is the only schema addition required for ``--for``. It mirrors ``config.ClusterCapabilities``: ``capabilities.nodes.{sriov, rdma, ib}`` describe what the underlying hardware can do, and ``FindApplicableProfile`` uses it to match a profile.

Adding New Presets
------------------

To add a preset for a new ``(machineType, gpuType)`` pair:

1. Run the topology collector on a representative node of that hardware combination.
2. Create a directory named to uniquely identify the variant (composite name recommended, e.g. ``PowerEdge-XE9680-B200``).
3. Place the ``topology.yaml`` in that directory with both ``machineType`` and ``gpuType`` declared.
4. If the preset should be usable with ``l8k generate --for``, add the ``capabilities.nodes`` block.
5. Submit a pull request to the repository.

The ``machineType`` string must match exactly what ``/sys/class/dmi/id/product_name`` returns (after space-to-dash sanitization), or the ``nvidia.com/gpu.machine`` node label value. The ``gpuType`` string must match the ``nvidia.com/gpu.product`` label or the sanitized ``nvidia-smi -q`` product name.
