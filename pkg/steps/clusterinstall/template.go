package clusterinstall

const installTemplateE2E = `
kind: Template
apiVersion: template.openshift.io/v1

parameters:
- name: JOB_NAME_SAFE
  required: true
- name: JOB_NAME_HASH
  required: true
- name: NAMESPACE
  required: true
- name: IMAGE_INSTALLER
  required: true
- name: IMAGE_TESTS
  required: true
- name: CLUSTER_TYPE
  required: true
- name: TEST_COMMAND
  required: true
- name: RELEASE_IMAGE_LATEST
  required: true
- name: BASE_DOMAIN
  value: origin-ci-int-aws.dev.rhcloud.com
  required: true

objects:

# We want the cluster to be able to access these images
- kind: RoleBinding
  apiVersion: authorization.openshift.io/v1
  metadata:
    name: ${JOB_NAME_SAFE}-image-puller
    namespace: ${NAMESPACE}
  roleRef:
    name: system:image-puller
  subjects:
  - kind: SystemGroup
    name: system:unauthenticated
  - kind: SystemGroup
    name: system:authenticated

# Give edit access to a known bot
- kind: RoleBinding
  apiVersion: authorization.openshift.io/v1
  metadata:
    name: ${JOB_NAME_SAFE}-namespace-editors
    namespace: ${NAMESPACE}
  roleRef:
    name: edit
  subjects:
  - kind: ServiceAccount
    namespace: ci
    name: ci-chat-bot

# The e2e pod spins up a cluster, runs e2e tests, and then cleans up the cluster.
- kind: Pod
  apiVersion: v1
  metadata:
    name: ${JOB_NAME_SAFE}
    namespace: ${NAMESPACE}
    annotations:
      # we want to gather the teardown logs no matter what
      ci-operator.openshift.io/wait-for-container-artifacts: teardown
      ci-operator.openshift.io/save-container-logs: "true"
      ci-operator.openshift.io/container-sub-tests: "setup,test,teardown"
  spec:
    restartPolicy: Never
    activeDeadlineSeconds: 14400
    terminationGracePeriodSeconds: 900
    volumes:
    - name: artifacts
      emptyDir: {}
    - name: shared-tmp
      emptyDir: {}
    - name: cluster-profile
      secret:
        secretName: ${JOB_NAME_SAFE}-cluster-profile

    containers:

    # Once the cluster is up, executes shared tests
    - name: test
      image: ${IMAGE_TESTS}
      terminationMessagePolicy: FallbackToLogsOnError
      resources:
        requests:
          cpu: 1
          memory: 300Mi
        limits:
          memory: 3Gi
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp/shared
      - name: cluster-profile
        mountPath: /tmp/cluster
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /tmp/cluster/.awscred
      - name: ARTIFACT_DIR
        value: /tmp/artifacts
      - name: HOME
        value: /tmp/home
      - name: KUBECONFIG
        value: /tmp/artifacts/installer/auth/kubeconfig
      command:
      - /bin/bash
      - -c
      - |
        #!/bin/bash
        set -euo pipefail

        export PATH=/usr/libexec/origin:$PATH

        trap 'touch /tmp/shared/exit' EXIT
        trap 'kill $(jobs -p); exit 0' TERM

        mkdir -p "${HOME}"

        # wait for the API to come up
        while true; do
          if [[ -f /tmp/shared/exit ]]; then
            echo "Another process exited" 2>&1
            exit 1
          fi
          if [[ ! -f /tmp/shared/setup-success ]]; then
            sleep 15 & wait
            continue
          fi
          # don't let clients impact the global kubeconfig
          cp "${KUBECONFIG}" /tmp/admin.kubeconfig
          export KUBECONFIG=/tmp/admin.kubeconfig
          break
        done

        until oc --insecure-skip-tls-verify wait clusterversion/version --for condition=available 2>/dev/null; do
          sleep 10 & wait
        done

        # set up cloud-provider-specific env vars
        export KUBE_SSH_BASTION="$( oc --insecure-skip-tls-verify get node -l node-role.kubernetes.io/master -o 'jsonpath={.items[0].status.addresses[?(@.type=="ExternalIP")].address}' ):22"
        export KUBE_SSH_KEY_PATH=/tmp/cluster/ssh-privatekey
        if [[ "${CLUSTER_TYPE}" == "gcp" ]]; then
          export GOOGLE_APPLICATION_CREDENTIALS="/tmp/cluster/gce.json"
          export KUBE_SSH_USER=cloud-user
          mkdir -p ~/.ssh
          cp /tmp/cluster/ssh-privatekey ~/.ssh/google_compute_engine || true
          export PROVIDER_ARGS='-provider=gce -gce-zone=us-east1-c -gce-project=openshift-gce-devel-ci'
          export TEST_PROVIDER='{"type":"gce","zone":"us-east1-c","projectid":"openshift-gce-devel-ci"}'
        elif [[ "${CLUSTER_TYPE}" == "aws" ]]; then
          mkdir -p ~/.ssh
          cp /tmp/cluster/ssh-privatekey ~/.ssh/kube_aws_rsa || true
          export PROVIDER_ARGS="-provider=aws -gce-zone=us-east-1"
          # TODO: make openshift-tests auto-discover this from cluster config
          export TEST_PROVIDER='{"type":"aws","region":"us-east-1","zone":"us-east-1a","multizone":true,"multimaster":true}'
          export KUBE_SSH_USER=core
        elif [[ "${CLUSTER_TYPE}" == "openstack" ]]; then
          mkdir -p ~/.ssh
          cp /tmp/cluster/ssh-privatekey ~/.ssh/kube_openstack_rsa || true
        fi

        mkdir -p /tmp/output
        cd /tmp/output

        function run-upgrade-tests() {
          openshift-tests run-upgrade "${TEST_SUITE}" --to-image "${RELEASE_IMAGE_LATEST}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          exit 0
        }

        function run-tests() {
          openshift-tests run "${TEST_SUITE}" \
            --provider "${TEST_PROVIDER:-}" -o /tmp/artifacts/e2e.log --junit-dir /tmp/artifacts/junit
          exit 0
        }

        ${TEST_COMMAND}

    # Runs an install
    - name: setup
      image: ${IMAGE_INSTALLER}
      terminationMessagePolicy: FallbackToLogsOnError
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp
      - name: cluster-profile
        mountPath: /etc/openshift-installer
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: TYPE
        value: ${CLUSTER_TYPE}
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /etc/openshift-installer/.awscred
      - name: AWS_REGION
        value: us-east-1
      - name: CLUSTER_NAME
        value: ${NAMESPACE}-${JOB_NAME_HASH}
      - name: BASE_DOMAIN
        value: ${BASE_DOMAIN}
      - name: SSH_PUB_KEY_PATH
        value: /etc/openshift-installer/ssh-publickey
      - name: PULL_SECRET_PATH
        value: /etc/openshift-installer/pull-secret
      - name: OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE
        value: ${RELEASE_IMAGE_LATEST}
      - name: OPENSTACK_IMAGE
        value: rhcos
      - name: OPENSTACK_REGION
        value: moc-kzn
      - name: OPENSTACK_FLAVOR
        value: m1.s2.xlarge
      - name: OPENSTACK_EXTERNAL_NETWORK
        value: external
      - name: OS_CLOUD
        value: openstack-cloud
      - name: OS_CLIENT_CONFIG_FILE
        value: /etc/openshift-installer/clouds.yaml
      - name: USER
        value: test
      - name: HOME
        value: /tmp
      - name: INSTALL_INITIAL_RELEASE
      - name: RELEASE_IMAGE_INITIAL
      command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        trap 'rc=$?; if test "${rc}" -eq 0; then touch /tmp/setup-success; else touch /tmp/exit; fi; exit "${rc}"' EXIT
        trap 'CHILDREN=$(jobs -p); if test -n "${CHILDREN}"; then kill ${CHILDREN} && wait; fi' TERM

        cp "$(command -v openshift-install)" /tmp
        mkdir /tmp/artifacts/installer

        if [[ -n "${INSTALL_INITIAL_RELEASE}" && -n "${RELEASE_IMAGE_INITIAL}" ]]; then
          echo "Installing from initial release ${RELEASE_IMAGE_INITIAL}"
          OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE="${RELEASE_IMAGE_INITIAL}"
        else
          echo "Installing from release ${RELEASE_IMAGE_LATEST}"
        fi

        export EXPIRATION_DATE=$(date -d '4 hours' --iso=minutes --utc)
        export SSH_PUB_KEY=$(cat "${SSH_PUB_KEY_PATH}")
        export PULL_SECRET=$(cat "${PULL_SECRET_PATH}")

        if [[ "${CLUSTER_TYPE}" == "aws" ]]; then
            cat > /tmp/artifacts/installer/install-config.yaml << EOF
        apiVersion: v1beta4
        baseDomain: ${BASE_DOMAIN}
        metadata:
          name: ${CLUSTER_NAME}
        controlPlane:
          name: master
          replicas: 3
          platform:
            aws:
              zones:
              - us-east-1a
              - us-east-1b
              - us-east-1c
        compute:
        - name: worker
          replicas: 3
          platform:
            aws:
              zones:
              - us-east-1a
              - us-east-1b
              - us-east-1c
        networking:
          clusterNetwork:
          - cidr: 10.128.0.0/14
            hostPrefix: 23
          machineCIDR: 10.0.0.0/16
          serviceNetwork:
          - 172.30.0.0/16
          networkType: OpenShiftSDN
        platform:
          aws:
            region:       ${AWS_REGION}
            userTags:
              expirationDate: ${EXPIRATION_DATE}
        pullSecret: >
          ${PULL_SECRET}
        sshKey: |
          ${SSH_PUB_KEY}
        EOF
        elif [[ "${CLUSTER_TYPE}" == "openshift" ]]; then
          # create a new floating ip tagged with CLUSTER_NAME so it can be deleted later
          LB_FIP=$(openstack floating ip create --description $CLUSTER_NAME $OPENSTACK_EXTERNAL_NETWORK --format value -c 'floating_ip_address')

          # create A record for the api
          curl -v -X POST $CI_DNS_IP:8080 -d '{"name": "'api.$CLUSTER_NAME'", "ip": "'$LB_FIP'"}'

          cat > /tmp/artifacts/installer/install-config.yaml << EOF
        apiVersion: v1beta4
        baseDomain: ${BASE_DOMAIN}
        metadata:
          name: ${CLUSTER_NAME}
        networking:
          clusterNetworks:
          - cidr:             10.128.0.0/14
            hostSubnetLength: 9
          machineCIDR: 10.0.0.0/16
          serviceCIDR: 172.30.0.0/16
          type:        OpenShiftSDN
        platform:
          openstack:
            baseImage:        ${OPENSTACK_IMAGE}
            cloud:            ${OS_CLOUD}
            externalNetwork:  ${OPENSTACK_EXTERNAL_NETWORK}
            computeFlavor:    ${OPENSTACK_FLAVOR}
            region:           ${OPENSTACK_REGION}
            lbFloatingIP:     ${LB_FIP}
        pullSecret: >
          ${PULL_SECRET}
        sshKey: |
          ${SSH_PUB_KEY}
        EOF
        else
            echo "Unsupported cluster type '${CLUSTER_NAME}'"
            exit 1
        fi

        TF_LOG=debug openshift-install --dir=/tmp/artifacts/installer create cluster &
        wait "$!"

    # Performs cleanup of all created resources
    - name: teardown
      image: ${IMAGE_TESTS}
      terminationMessagePolicy: FallbackToLogsOnError
      volumeMounts:
      - name: shared-tmp
        mountPath: /tmp/shared
      - name: cluster-profile
        mountPath: /etc/openshift-installer
      - name: artifacts
        mountPath: /tmp/artifacts
      env:
      - name: INSTANCE_PREFIX
        value: ${NAMESPACE}-${JOB_NAME_HASH}
      - name: TYPE
        value: ${CLUSTER_TYPE}
      - name: KUBECONFIG
        value: /tmp/artifacts/installer/auth/kubeconfig
      command:
      - /bin/bash
      - -c
      - |
        #!/bin/bash
        function queue() {
          local TARGET="${1}"
          shift
          local LIVE="$(jobs | wc -l)"
          while [[ "${LIVE}" -ge 45 ]]; do
            sleep 1
            LIVE="$(jobs | wc -l)"
          done
          echo "${@}"
          if [[ -n "${FILTER}" ]]; then
            "${@}" | "${FILTER}" >"${TARGET}" &
          else
            "${@}" >"${TARGET}" &
          fi
        }

        function teardown() {
          set +e
          touch /tmp/shared/exit
          export PATH=$PATH:/tmp/shared

          echo "Gathering artifacts ..."
          mkdir -p /tmp/artifacts/pods /tmp/artifacts/nodes /tmp/artifacts/metrics /tmp/artifacts/bootstrap /tmp/artifacts/network


          if [ -f /tmp/artifacts/installer/terraform.tfstate ]
          then
              # we don't have jq, so the python equivalent of
              # jq '.modules[].resources."aws_instance.bootstrap".primary.attributes."public_ip" | select(.)'
              bootstrap_ip=$(python -c \
                  'import sys, json; d=reduce(lambda x,y: dict(x.items() + y.items()), map(lambda x: x["resources"], json.load(sys.stdin)["modules"])); k="aws_instance.bootstrap"; print d[k]["primary"]["attributes"]["public_ip"] if k in d else ""' \
                  < /tmp/artifacts/installer/terraform.tfstate
              )

              if [ -n "${bootstrap_ip}" ]
              then
                for service in bootkube openshift kubelet crio
                do
                    queue "/tmp/artifacts/bootstrap/${service}.service" curl \
                        --insecure \
                        --silent \
                        --connect-timeout 5 \
                        --retry 3 \
                        --cert /tmp/artifacts/installer/tls/journal-gatewayd.crt \
                        --key /tmp/artifacts/installer/tls/journal-gatewayd.key \
                        --url "https://${bootstrap_ip}:19531/entries?_SYSTEMD_UNIT=${service}.service"
                done
                if ! whoami &> /dev/null; then
                  if [ -w /etc/passwd ]; then
                    echo "${USER_NAME:-default}:x:$(id -u):0:${USER_NAME:-default} user:${HOME}:/sbin/nologin" >> /etc/passwd
                  fi
                fi
                eval $(ssh-agent)
                ssh-add /etc/openshift-installer/ssh-privatekey
                ssh -A -o PreferredAuthentications=publickey -o StrictHostKeyChecking=false -o UserKnownHostsFile=/dev/null core@${bootstrap_ip} /bin/bash -x /usr/local/bin/installer-gather.sh
                scp -o PreferredAuthentications=publickey -o StrictHostKeyChecking=false -o UserKnownHostsFile=/dev/null core@${bootstrap_ip}:log-bundle.tar.gz /tmp/artifacts/installer/bootstrap-logs.tar.gz
              fi
          else
              echo "No terraform statefile found. Skipping collection of bootstrap logs."
          fi

          oc --insecure-skip-tls-verify --request-timeout=5s get nodes -o jsonpath --template '{range .items[*]}{.metadata.name}{"\n"}{end}' > /tmp/nodes
          oc --insecure-skip-tls-verify --request-timeout=5s get pods --all-namespaces --template '{{ range .items }}{{ $name := .metadata.name }}{{ $ns := .metadata.namespace }}{{ range .spec.containers }}-n {{ $ns }} {{ $name }} -c {{ .name }}{{ "\n" }}{{ end }}{{ range .spec.initContainers }}-n {{ $ns }} {{ $name }} -c {{ .name }}{{ "\n" }}{{ end }}{{ end }}' > /tmp/containers
          oc --insecure-skip-tls-verify --request-timeout=5s get pods -l openshift.io/component=api --all-namespaces --template '{{ range .items }}-n {{ .metadata.namespace }} {{ .metadata.name }}{{ "\n" }}{{ end }}' > /tmp/pods-api

          queue /tmp/artifacts/config-resources.json oc --insecure-skip-tls-verify --request-timeout=5s get apiserver.config.openshift.io authentication.config.openshift.io build.config.openshift.io console.config.openshift.io dns.config.openshift.io featuregate.config.openshift.io image.config.openshift.io infrastructure.config.openshift.io ingress.config.openshift.io network.config.openshift.io oauth.config.openshift.io project.config.openshift.io scheduler.config.openshift.io -o json
          queue /tmp/artifacts/apiservices.json oc --insecure-skip-tls-verify --request-timeout=5s get apiservices -o json
          queue /tmp/artifacts/clusteroperators.json oc --insecure-skip-tls-verify --request-timeout=5s get clusteroperators -o json
          queue /tmp/artifacts/clusterversion.json oc --insecure-skip-tls-verify --request-timeout=5s get clusterversion -o json
          queue /tmp/artifacts/configmaps.json oc --insecure-skip-tls-verify --request-timeout=5s get configmaps --all-namespaces -o json
          queue /tmp/artifacts/credentialsrequests.json oc --insecure-skip-tls-verify --request-timeout=5s get credentialsrequests --all-namespaces -o json
          queue /tmp/artifacts/csr.json oc --insecure-skip-tls-verify --request-timeout=5s get csr -o json
          queue /tmp/artifacts/endpoints.json oc --insecure-skip-tls-verify --request-timeout=5s get endpoints --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/deployments.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get deployments --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/daemonsets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get daemonsets --all-namespaces -o json
          queue /tmp/artifacts/events.json oc --insecure-skip-tls-verify --request-timeout=5s get events --all-namespaces -o json
          queue /tmp/artifacts/kubeapiserver.json oc --insecure-skip-tls-verify --request-timeout=5s get kubeapiserver -o json
          queue /tmp/artifacts/kubecontrollermanager.json oc --insecure-skip-tls-verify --request-timeout=5s get kubecontrollermanager -o json
          queue /tmp/artifacts/machineconfigpools.json oc --insecure-skip-tls-verify --request-timeout=5s get machineconfigpools -o json
          queue /tmp/artifacts/machineconfigs.json oc --insecure-skip-tls-verify --request-timeout=5s get machineconfigs -o json
          queue /tmp/artifacts/namespaces.json oc --insecure-skip-tls-verify --request-timeout=5s get namespaces -o json
          queue /tmp/artifacts/nodes.json oc --insecure-skip-tls-verify --request-timeout=5s get nodes -o json
          queue /tmp/artifacts/openshiftapiserver.json oc --insecure-skip-tls-verify --request-timeout=5s get openshiftapiserver -o json
          queue /tmp/artifacts/pods.json oc --insecure-skip-tls-verify --request-timeout=5s get pods --all-namespaces -o json
          queue /tmp/artifacts/persistentvolumes.json oc --insecure-skip-tls-verify --request-timeout=5s get persistentvolumes --all-namespaces -o json
          queue /tmp/artifacts/persistentvolumeclaims.json oc --insecure-skip-tls-verify --request-timeout=5s get persistentvolumeclaims --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/replicasets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get replicasets --all-namespaces -o json
          queue /tmp/artifacts/rolebindings.json oc --insecure-skip-tls-verify --request-timeout=5s get rolebindings --all-namespaces -o json
          queue /tmp/artifacts/roles.json oc --insecure-skip-tls-verify --request-timeout=5s get roles --all-namespaces -o json
          queue /tmp/artifacts/services.json oc --insecure-skip-tls-verify --request-timeout=5s get services --all-namespaces -o json
          FILTER=gzip queue /tmp/artifacts/statefulsets.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get statefulsets --all-namespaces -o json

          FILTER=gzip queue /tmp/artifacts/openapi.json.gz oc --insecure-skip-tls-verify --request-timeout=5s get --raw /openapi/v2

          # gather nodes first in parallel since they may contain the most relevant debugging info
          while IFS= read -r i; do
            mkdir -p /tmp/artifacts/nodes/$i
            queue /tmp/artifacts/nodes/$i/heap oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/debug/pprof/heap
          done < /tmp/nodes

          if oc --insecure-skip-tls-verify adm node-logs -h &>/dev/null; then
            # starting in 4.0 we can query node logs directly
            FILTER=gzip queue /tmp/artifacts/nodes/masters-journal.gz oc --insecure-skip-tls-verify adm node-logs --role=master --unify=false
            FILTER=gzip queue /tmp/artifacts/nodes/workers-journal.gz oc --insecure-skip-tls-verify adm node-logs --role=worker --unify=false
          else
            while IFS= read -r i; do
              FILTER=gzip queue /tmp/artifacts/nodes/$i/messages.gz oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/logs/messages
              oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/logs/journal | sed -e 's|.*href="\(.*\)".*|\1|;t;d' > /tmp/journals
              while IFS= read -r j; do
                FILTER=gzip queue /tmp/artifacts/nodes/$i/journal.gz oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/logs/journal/${j}system.journal
              done < /tmp/journals
              FILTER=gzip queue /tmp/artifacts/nodes/$i/secure.gz oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/logs/secure
              FILTER=gzip queue /tmp/artifacts/nodes/$i/audit.gz oc --insecure-skip-tls-verify get --request-timeout=20s --raw /api/v1/nodes/$i/proxy/logs/audit
            done < /tmp/nodes
          fi

          # Snapshot iptables-save on each node for debugging possible kube-proxy issues
          oc --insecure-skip-tls-verify get --request-timeout=20s -n openshift-sdn -l app=sdn pods --template '{{ range .items }}{{ .metadata.name }}{{ "\n" }}{{ end }}' > /tmp/sdn-pods
          while IFS= read -r i; do
            queue /tmp/artifacts/network/iptables-save-$i oc --insecure-skip-tls-verify rsh --timeout=20 -n openshift-sdn -c sdn $i iptables-save -c
          done < /tmp/sdn-pods

          while IFS= read -r i; do
            file="$( echo "$i" | cut -d ' ' -f 3 | tr -s ' ' '_' )"
            queue /tmp/artifacts/metrics/${file}-heap oc --insecure-skip-tls-verify exec $i -- /bin/bash -c 'oc --insecure-skip-tls-verify get --raw /debug/pprof/heap --server "https://$( hostname ):8443" --config /etc/origin/master/admin.kubeconfig'
            queue /tmp/artifacts/metrics/${file}-controllers-heap oc --insecure-skip-tls-verify exec $i -- /bin/bash -c 'oc --insecure-skip-tls-verify get --raw /debug/pprof/heap --server "https://$( hostname ):8444" --config /etc/origin/master/admin.kubeconfig'
          done < /tmp/pods-api

          while IFS= read -r i; do
            file="$( echo "$i" | cut -d ' ' -f 2,3,5 | tr -s ' ' '_' )"
            FILTER=gzip queue /tmp/artifacts/pods/${file}.log.gz oc --insecure-skip-tls-verify logs --request-timeout=20s $i
            FILTER=gzip queue /tmp/artifacts/pods/${file}_previous.log.gz oc --insecure-skip-tls-verify logs --request-timeout=20s -p $i
          done < /tmp/containers

          echo "Gathering kube-apiserver audit.log ..."
          oc --insecure-skip-tls-verify adm node-logs --role=master --path=kube-apiserver/ > /tmp/kube-audit-logs
          while IFS=$'\n' read -r line; do
            IFS=' ' read -ra log <<< "${line}"
            FILTER=gzip queue /tmp/artifacts/nodes/"${log[0]}"-"${log[1]}".gz oc --insecure-skip-tls-verify adm node-logs "${log[0]}" --path=kube-apiserver/"${log[1]}"
          done < /tmp/kube-audit-logs

          echo "Gathering openshift-apiserver audit.log ..."
          oc --insecure-skip-tls-verify adm node-logs --role=master --path=openshift-apiserver/ > /tmp/openshift-audit-logs
          while IFS=$'\n' read -r line; do
            IFS=' ' read -ra log <<< "${line}"
            FILTER=gzip queue /tmp/artifacts/nodes/"${log[0]}"-"${log[1]}".gz oc --insecure-skip-tls-verify adm node-logs "${log[0]}" --path=openshift-apiserver/"${log[1]}"
          done < /tmp/openshift-audit-logs

          echo "Snapshotting prometheus (may take 15s) ..."
          queue /tmp/artifacts/metrics/prometheus.tar.gz oc --insecure-skip-tls-verify exec -n openshift-monitoring prometheus-k8s-0 -- tar cvzf - -C /prometheus .

          echo "Running must-gather..."
          mkdir -p /tmp/artifacts/must-gather
          queue /tmp/artifacts/must-gather/must-gather.log oc --insecure-skip-tls-verify adm must-gather --dest-dir /tmp/artifacts/must-gather

          echo "Waiting for logs ..."
          wait

          echo "Deprovisioning cluster ..."
          export AWS_SHARED_CREDENTIALS_FILE=/etc/openshift-installer/.awscred
          openshift-install --dir /tmp/artifacts/installer destroy cluster
        }

        trap 'teardown' EXIT
        trap 'kill $(jobs -p); exit 0' TERM

        for i in $(seq 1 180); do
          if [[ -f /tmp/shared/exit ]]; then
            exit 0
          fi
          sleep 60 & wait
        done
`
