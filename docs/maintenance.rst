Maintenance and upgrade concurrency
===================================

The ``maintenance`` section controls how many nodes l8k allows the NVIDIA
operators to process simultaneously. It applies to host-cluster Network
Operator profiles and defaults to four concurrent nodes so large clusters do
not inherit the upstream single-node defaults.

Configuration
-------------

.. code-block:: yaml

   maintenance:
     maxParallelOperations: 4
     maxUnavailable: 4
     maxNodeMaintenanceTimeSeconds: 3600
     maxParallelUpgrades: 4

If the section or an individual field is omitted, l8k fills that field with
the default shown above.

Fields and restrictions
-----------------------

.. list-table::
   :widths: 24 13 63
   :header-rows: 1

   * - Field
     - Default
     - Meaning and accepted values
   * - ``maxParallelOperations``
     - ``4``
     - Global Maintenance Operator work limit. Use a positive integer or a
       percentage string from ``"1%"`` through ``"100%"``. Although the CRD
       accepts integer ``0``, the current scheduler calculates zero available
       slots, so l8k requires a positive value. A percentage is calculated
       from all cluster nodes and rounded up.
   * - ``maxUnavailable``
     - ``4``
     - Maximum unavailable nodes. Use a non-negative integer or a percentage
       string from ``"1%"`` through ``"100%"``. Integer ``0`` pauses new
       maintenance work. In requestor mode, nodes that are already cordoned or
       NotReady consume this budget, even when another controller made them
       unavailable, and a percentage is calculated from all cluster nodes and
       rounded down. On the legacy SR-IOV path it is calculated from nodes in
       the selected pool and also rounded down; a small percentage can
       therefore resolve to zero.
   * - ``maxNodeMaintenanceTimeSeconds``
     - ``3600``
     - Non-negative cleanup delay for a ``NodeMaintenance`` request after it
       reaches Ready. ``0`` makes a Ready request immediately eligible for
       garbage collection; it does not disable cleanup and is not an operation
       timeout. Keep it below the idle interval of any cluster autoscaler.
   * - ``maxParallelUpgrades``
     - ``4``
     - Non-negative OFED upgrade limit for Network Operator releases older
       than 26.1. ``0`` means unlimited on that legacy path. Network Operator
       requestor mode does not consult this field, so it has no effect on OFED
       concurrency starting with release 26.1.

Integer-or-percentage fields must be YAML integer scalars or strings ending in
``%``. Numeric strings such as ``"4"``, fractional numbers such as ``1.5``,
and percentages outside ``1%`` through ``100%`` are rejected.

Two Maintenance Operator limits apply together: a request starts only while
both ``maxParallelOperations`` and ``maxUnavailable`` have capacity. For
example, with ``maxUnavailable: 4`` and two nodes already cordoned or NotReady,
at most two additional nodes can become unavailable.

Release-specific behavior
-------------------------

l8k gates requestor mode on ``networkOperator.selectedRelease``. Release 26.1
and newer, as well as an empty release interpreted as latest, use requestor
mode. Older releases retain the operators' internal drain paths.

.. list-table::
   :widths: 23 35 42
   :header-rows: 1

   * - Flow
     - Before Network Operator 26.1
     - Network Operator 26.1 and newer
   * - DOCA/OFED upgrade
     - Network Operator drains nodes directly.
       ``maxParallelUpgrades`` is the effective limit.
     - The generated Helm values set
       ``operator.maintenanceOperator.useRequestor: true``. Network Operator
       creates ``NodeMaintenance`` requests; the global Maintenance Operator
       limits are effective and ``maxParallelUpgrades`` is ignored.
   * - SR-IOV configuration
     - The SR-IOV Operator internal drain controller is active.
       ``SriovNetworkPoolConfig.spec.maxUnavailable`` is the effective pool
       limit. Pod disruption budgets are still respected when the limit is
       greater than one.
     - The generated Helm values enable both
       ``operator.maintenanceOperator.useDrainControllerRequestor`` and
       ``sriov-network-operator.operator.externalDrainer.enabled``. Both are
       required to hand draining from the SR-IOV internal controller to the
       Network Operator requestor. The global Maintenance Operator limits are
       effective; ``SriovNetworkPoolConfig.spec.maxUnavailable`` no longer
       controls draining.

The generated ``MaintenanceOperatorConfig`` receives
``maxParallelOperations``, ``maxUnavailable``, and
``maxNodeMaintenanceTimeSeconds``. In legacy mode, l8k also wires
``maxUnavailable`` to ``SriovNetworkPoolConfig`` and
``maxParallelUpgrades`` to the OFED upgrade policy. The same flat config can
therefore be used across release upgrades while l8k selects the applicable
controller.

l8k does not add ``network.nvidia.com/operator.nic-configuration.wait`` to
the SR-IOV config daemon's node selector. The daemon retains the Network
Operator chart's default scheduling selectors; drain sequencing remains
controlled by the release-appropriate SR-IOV drain path described above.

Upgrading an existing installation
-----------------------------------

Requestor mode is partly configured through Network Operator Helm values. The
chart renders those values into environment variables on the Network Operator
Deployment and into the SR-IOV Operator subchart. Applying only the generated
custom resources out of band cannot enable these requestors.

When an existing Helm release has different values, regenerate the deployment
for the selected release and deploy with ``--overwrite-existing`` so l8k runs
``helm upgrade --install``. Without that flag, l8k intentionally reports the
values conflict instead of changing an existing release.

.. code-block:: bash

   l8k generate --user-config cluster-config.yaml \
     --network-operator-release 26.1 \
     --fabric ethernet --deployment-type sriov \
     --save-deployment-files ./deployment \
     --deploy --overwrite-existing --kubeconfig ~/.kube/config

After the upgrade, verify the Network Operator Deployment and the
``MaintenanceOperatorConfig`` before starting disruptive work. Do not enable
only one of the two SR-IOV requestor settings: the external drainer and the
Network Operator drain requestor are a coordinated handoff.
