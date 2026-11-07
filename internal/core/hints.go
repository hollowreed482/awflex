package core

import (
	"regexp"
	"strings"
)

// A hint names the likely cause of a known failure and the exact fix, so an
// operator (often an automated agent) does not have to search for it. Hints
// are matched against run errors, failed task messages and doctor output.
type hint struct {
	re  *regexp.Regexp
	fix string
}

var hints = []hint{
	{regexp.MustCompile(`(?i)no user exists for uid`),
		"ssh needs a passwd entry for the user the pod runs as. Build the image with deploy/Containerfile.base, which adds one for uid 1000, and keep runAsUser: 1000. Do not run as root to work around it. Check with: awflex doctor"},
	{regexp.MustCompile(`(?i)invalid callback for stdout|could not load .*callback|callback plugin .* (not found|could not be loaded)`),
		"the repo's ansible.cfg or the pod's ANSIBLE_* settings name a callback plugin this image does not have. awflex sets ANSIBLE_STDOUT_CALLBACK=default itself; remove any ANSIBLE_STDOUT_CALLBACK from the pod env, or install the collection that provides the callback in the image. Do not edit awflex's source."},
	{regexp.MustCompile(`(?i)couldn't resolve module/action|the role '.*' was not found|unable to find .* in collection|no module named`),
		"a collection, role or Python module the playbook uses is missing from the image. Build with deploy/Containerfile.base on the ansible image your playbooks already run in, or add it at build time (see Containerfile ANSIBLE_PACKAGES). Check with: awflex doctor"},
	{regexp.MustCompile(`(?is)git (fetch|init).*(authentication failed|could not read username|terminal prompts disabled|permission denied \(publickey)`),
		"git could not authenticate to AWFLEX_REPO. Store git_token (with git_user) or git_ssh_key: awflex cred set git --git-user U --git-token-file F. Check with: awflex doctor"},
	{regexp.MustCompile(`(?i)does not appear to be a git repository|repository '.*' not found|could not read from remote repository|unable to access '`),
		"git cannot read AWFLEX_REPO. Check the URL is right and reachable from the pod (kubectl exec deploy/awflex -- git ls-remote URL), and that a private repo has a git credential: awflex cred set git --git-user U --git-token-file F"},
	{regexp.MustCompile(`(?i)(ssl certificate problem|certificate verify failed|x509: certificate signed by unknown authority)`),
		"a TLS certificate is signed by a CA the image does not trust. Build with deploy/Containerfile.base on an image that has your CA bundle, or add the CA to the image's trust store."},
	{regexp.MustCompile(`(?i)branch/ref .* not found`),
		"the branch, tag or commit does not exist in AWFLEX_REPO. Push it, or fix the name the caller sent. Check the default with: awflex doctor"},
	{regexp.MustCompile(`(?i)playbook .* not found in`),
		"the playbook path is relative to the repo root and must exist on that branch. Fix AWFLEX_PLAYBOOK or the playbook the caller sent. Check with: awflex doctor"},
	{regexp.MustCompile(`(?i)unable to parse .* as an inventory source|no inventory was parsed|inventory sources returned no hosts|refreshing it failed|ansible-inventory at `),
		"the inventory sources could not be read. Check that the inventory plugin's collection is in the image, that its config file is in the repo on AWFLEX_BRANCH, and that its token is in a credential: awflex cred set inventory --inventory-env-file NAME=FILE. Then: awflex inventory refresh"},
	{regexp.MustCompile(`(?i)no tasks ran for this host|leaves us with no hosts to target|could not match supplied host pattern`),
		"the host is not in the inventory, or the play's hosts: pattern does not include it. Check with: awflex doctor --host HOST. With a generated inventory, set AWFLEX_GROUPS to the group the play targets."},
	{regexp.MustCompile(`(?i)permission denied \(publickey`),
		"the node refused the SSH key. Check awflex cred ls shows ssh_private_key and the right ssh_user, and that the public key is installed on the node (and on any jump host). Check with: awflex doctor --host HOST"},
	{regexp.MustCompile(`(?i)host key verification failed`),
		"an ssh hop checked a host key it does not know, usually the nested ssh inside a ProxyCommand. Add -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null to that inner ssh command in the inventory's ansible_ssh_common_args."},
	{regexp.MustCompile(`(?i)could not resolve hostname|name or service not known|temporary failure in name resolution`),
		"the pod cannot resolve the node's name. Set AWFLEX_INVENTORY to your real inventory, where each host has ansible_host; or add the node domain to the pod's DNS search list (dnsConfig in deploy/k8s/awflex.yaml); or call with a name the cluster's DNS knows."},
	{regexp.MustCompile(`(?i)connection timed out|no route to host|connection refused|operation timed out`),
		"the pod cannot reach the node's ssh port. Check network policy and firewalls from the pod, or route through a jump host with ProxyJump or ProxyCommand in ansible_ssh_common_args."},
	{regexp.MustCompile(`(?i)could not open a connection to your authentication agent|agent refused operation|error connecting to agent`),
		"the per-run ssh-agent is not usable. The image needs ssh-agent and ssh-add, or set AWFLEX_SSH_AGENT=0 when no jump host needs agent forwarding."},
	{regexp.MustCompile(`(?i)read-only file system`),
		"something wrote outside the writable mounts. HOME and TMPDIR must point at the /run/awflex tmpfs (the images set this) and /data must be a mounted volume."},
	{regexp.MustCompile(`(?i)(missing sudo password|a password is required|incorrect sudo password)`),
		"become needs a password. Store it: awflex cred set become --become-password-file F"},
	{regexp.MustCompile(`(?i)(attempting to decrypt but no vault secrets found|decryption failed)`),
		"the playbook uses vault-encrypted data. Store the vault password: awflex cred set vault --vault-password-file F"},
	{regexp.MustCompile(`(?i)likely out of memory`),
		"ansible was killed, usually by the pod's memory limit. Lower AWFLEX_MAX_FORKS or raise the memory limit; measure one run of the playbook to size them together."},
}

// hintFor returns the fix for the first known failure found in texts.
func hintFor(texts ...string) string {
	for _, h := range hints {
		for _, t := range texts {
			if t != "" && h.re.MatchString(t) {
				return h.fix
			}
		}
	}
	return ""
}

// runHint looks at everything a finished run recorded for a host.
func runHint(msg string, hr *hostResult) string {
	texts := []string{msg}
	if hr != nil {
		for _, ev := range hr.events {
			if ev.Status == "failed" || ev.Status == "unreachable" {
				texts = append(texts, ev.Msg, ev.Stderr)
			}
		}
	}
	return hintFor(strings.Join(texts, "\n"))
}
