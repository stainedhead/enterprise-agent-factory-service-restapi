# Changelog

All notable changes to this repository. `-db` and `-restapi` share version numbers (lockstep).

## v1.0.1

Paperclip team collaboration (spec `specs/archive/261122-paperclip-collaboration/`, change PRD `paperclip-collaboration-PRD.md`):

- API contract v1.3.1: seven collaboration operations (`GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`,
  `ConnectAgentToCollaboration`, `DisconnectAgentFromCollaboration`, `RotateCollaborationKeys`, `ListCollaborationEndpoints`), 189 operations in all.
- Role `team_collaborator` and permissions `collaboration:read|manage|access|contribute|api`.
- Collaboration agent (`role: collaboration`) with AFS-generated boot secrets; managed secrets (`managedBy`, `managed_secret`, reserved names).
- `AgentTemplate.capabilities`, `Team.mayExposeCollaboration`, identity provider `collabClientId` and `collabClientSecretArn`.
- Reconciler: Paperclip client, managed registration (agent create, key to Secrets Manager, pause, revoke), roster guard, readiness probe with open sign-up, agent-write scan, first-admin invite by email, member sync, `CollaborationKeyMissing`.
- Reports: `collaboration` kind (sign-ins, people, requests, denied requests, secret detections). Export/import carries collaboration settings and connect choices, never Paperclip data or managed keys.
- Adopts `-db` migration 000020 (pseudo-version for now; move to `-db` v1.0.1 once tagged).

## v1.0.0

Feature complete: `afs-api`, `afs-reconciler`, `afs-gateway` and `afs-billing` on `-db` migrations 000001 to 000019.
