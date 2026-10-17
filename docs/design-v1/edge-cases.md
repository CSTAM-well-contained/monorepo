When you delete the sandbox, Neutron releases 10.20.0.68 back to the subnet's pool immediately.
The next port created on that subnet, possibly team B's sandbox a second later during the stress test, can get 10.20.0.68.
Now the stale route from step 1 sends team A's subdomain to team B's app. Nothing has crashed, but traffic is going to the wrong team, which is a security bug.