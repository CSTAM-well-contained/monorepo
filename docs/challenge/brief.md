# Challenge Title: The Resilient IP Optimizer

**Challenge Reference:** CSTAM-FELCLOUD

**Company Collaborator:** FelCloud

**Assigned Mentor:**
- Mr. Mohamed Mongi Benyaiche - CEO FelCloud
- Mr. Oussama Zied - FelCloud Cloud & Devops Engineer

## 1. Company Introduction & Challenge Context

FelCloud operates in the Cloud Hosting sector, specializing in virtualized environments and developer sandbox automation. To drive digital transformation, we continuously explore innovations in Automated Edge Routing, High Availability Gateways, and Dynamic IP Management.
In today's rapidly evolving landscape, where dynamic cloud scaling is increasingly critical, we currently rely on static subnets and manual reverse proxies. As operations scale during high-density hackathons, we are actively seeking innovative approaches to eliminate single points of failure and automate dynamic IP recycling.

## 2. Problem Statement

Our main challenge is IPv4 address scarcity and gateway single points of failure. When multiple teams deploy sandboxes simultaneously or a gateway configuration fails, our systems experience network downtime and IP conflicts. We require a robust solution capable of automatically provisioning sandboxes, routing subdomains across redundant HA gateways with rollback capabilities, and aggressively recycling private IPs without disrupting existing infrastructure.

## 3. Technical Mission

**Challenge Objective:**
Participating teams must build a system evaluated across the following core phases:

### Phase 1: Core Functionalities MVP (Mandatory)

Total score: 50 points

Required Features:
- **Automated OpenStack Sandbox Provisioning [15 Points]:** API-triggered auto-provisioning of isolated student VMs/containers on OpenStack with dynamically assigned private IPs.
- **Dual Redundant HA Gateways [15 Points]:** Active/Standby or Active/Active edge gateways (Keepalived/VRRP/Octavia/HAProxy) routing incoming subdomains (e.g., team5.cstam.felcloud.tn) to private sandboxes with instant, seamless failover.
- **Automated IPAM & Reclamation Engine [10 Points]:** Managed OpenStack IP pool allocation with short leases and immediate release upon sandbox teardown or expiration.
- **Zero-Downtime Reload & Auto-Rollback [10 Points]:** Hot-reloading of routing rules with automated syntax validation and sub-second auto-rollback upon corrupt or invalid configurations.

Required Deliverables:
- Source code repository
- System Architecture & OpenStack HA Gateway Data Flow Diagram
- Short video demonstration showcasing active subdomain routing, dynamic hot-reloading, and simulated primary gateway failover

### Phase 2: Integration & Testing

Total score: 30 Points

Required Features:
- **Concurrent Provisioning Stress Test [15 Points]:** Rapid, error-free sandbox provisioning, IP allocation, and routing updates for 10+ simultaneous team registrations.
- **Automated Teardown & Lifecycle Security [10 Points]:** Automated cleanup pipeline purging expired sandboxes, proxy/DNS rules, and sanitizing IP allocations.
- **Network Isolation & Access Safeguards [5 Points]:** Neutron Security Groups/iptables/eBPF rules blocking inter-sandbox traffic and protecting the gateway management layer from sandbox access.

Required Deliverables:
- Benchmark report covering provisioning latency, gateway failover time, IP recycling speed, and load test metrics.
- Comprehensive API integration and security architecture documentation.

### Phase 3: UI/UX & Documentation

Total score: 10 points

Required Features:
- **Administrator Management Dashboard [10 Points]:** Intuitive admin web dashboard displaying:
  - Active team sandboxes & assigned OpenStack private IPs
  - Real-time gateway health status (Primary vs. Standby)
  - Mapped subdomains & live traffic logs
  - Manual lifecycle controls (spin-up, teardown, manual gateway failover, and manual config rollback)

Required Deliverables:
- Administrative dashboard interface and frontend source code
- Technical report documenting the project, implementation, and results
- A demonstration video showcasing the project's features and functionality

Note:
- **Only the selected teams will advance to the Final Pitching phase.**
- **The detailed selection criteria are provided in the General CSTAM 3.0 Challenge Guide**

### Phase 4: Pitching

Total score: 10 Points

- Present the completed solution to FelCloud technical representatives and the jury committee during the CSTAM 3.0 event.

Required Deliverables:
- A professional presentation supporting the pitch
- A live demonstration (including simulated primary gateway crash & auto-failover on OpenStack)
- Q&A session with the jury members

### Optional Advanced Features

Bonus Points

- **Automated SSL/TLS [+2 Points]:** Auto-issuance of wildcard or Let's Encrypt certificates for team subdomains.
- **Rate Limiting & Bandwidth Throttling [+2 Points]:** Gateway-level request capping per team.
- **Custom Domain Binding [+2 Points]:** Allow teams to map custom external domains to sandboxes.
- **Smart Health Monitoring & Fallback [+2 Points]:** Graceful maintenance/offline page when a sandbox crashes.
- **OpenStack Native Orchestration [+2 Points]:** Integration with OpenStack Heat, Designate (DNS), or eBPF/XDP filtering.

## 4. Recommended Skills

While you are highly encouraged to innovate and bring your own ideas to the table, your team will have a strong head start if you are familiar with:

- **Cloud & Infra:** OpenStack (Nova, Neutron, Keystone, Octavia, Heat), KVM, Docker, Podman
- **Gateways & Routing:** Nginx, Traefik, HAProxy, Keepalived (VRRP), Open vSwitch, BGP
- **Backend & API:** Python (FastAPI/Flask), Go, Node.js, Rust
- **Frontend:** React, Vue.js, TailwindCSS
- **Networking:** OpenStack Neutron Security Groups, CoreDNS/Designate, iptables/eBPF

## 5. Rewards & Opportunities

- **Number of Winners:** 3 teams
- Awards
  - **First Place Team:** PFE 2027 opportunities at FelCloud
  - **Second Place Team:** Certificate of Recognition
  - **Third Place Team:** Certificate of Recognition

Note:
- **Only the selected teams will advance to the Final Pitching phase.**
- **The detailed selection criteria are provided in the General CSTAM 3.0 Challenge Guide**
