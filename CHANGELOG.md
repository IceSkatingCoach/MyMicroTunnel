# Changelog

Sparkle shows the entry for a release in the update dialog, so what is written
here is what a user reads when deciding whether to install. Write for them:
what changed and whether it matters, not which functions moved.

The format is one `## <version>` heading per release, newest first.
`make appcast` reads the section matching `VERSION` and embeds it in the feed.

## 1.5.9

- The option to keep the tunnel as you left it across restarts and sleep
  now works. In earlier versions the background service that does this
  looked for your VPN profiles in the wrong place, found none, and did
  nothing, so after a restart the tunnel stayed down until you switched it
  on again. If you turned this option on during setup, it starts working
  when you update. Nothing else needs to change.
- Runs on Linux. A Debian 13 bundle with an install script is at
  https://downloads.maragato.ca/linux/. Linux has no menu bar, so you raise
  and drop the tunnel from the command line; everything else works as it
  does on a Mac.

## 1.5.8

- Installable with Homebrew: `brew tap IceSkatingCoach/mymicrotunnel` then
  `brew install --cask mymicrotunnel`. The cask installs the same signed,
  notarized package this feed serves, so an install from either side updates
  from the other.
- The one-click install link no longer carries the vendor's AWS account
  number. CloudFormation will not fetch a template through a CDN, so that
  link has to name an S3 bucket directly, and the bucket it named was the
  site's — whose name ends in the account id. The template now lives in a
  bucket of its own called mymicrotunnel-launch. Nothing about your own
  deployment changes; the old link still works.

## 1.5.7

- Fixed: a second deployment into the same VPC never served anything. Its
  tunnel came up and looked healthy, but the load balancer's health check
  was answered through the first deployment's tunnel — a workstation can
  hold one route for the VPC, and the first deployment had it. The gateway
  now translates what it forwards to its own tunnel address, so replies stay
  in the tunnel they arrived on and any number of deployments can share a
  VPC. Deploy again to pick it up: the gateway is replaced.
- A "Donate to us" section in setup, with a QR code and a link. This is GPL
  software and stays that way; what donations pay for is the AWS account the
  end-to-end tests deploy into and the compliance verification each release
  goes through.
- Fixed: the port hint still said a bare service port is published on 443. It
  is published under its own name, which is what the engine has always done.
- Fixed: runs of blank space in the middle of sentences, in the first-run
  instructions, the port hint and the new donation panel. The wrapped lines
  of those texts had been joined without their line breaks.

## 1.5.6

- Fixed: saving a new port deployed the old one. Saving in the setup window
  runs a CloudFormation update, and that update read the settings the
  previous run had left in the temporary directory rather than the profile
  it was told to deploy — so the stack changed and the port selection was
  ignored. What is recorded under the named profile now wins.
- Fixed: publishing the service on 443 would not stick. The setup window
  showed a service port of 3000:443 as plain "3000", so the next save wrote
  the published port back to 3000 and the hostname stopped answering without
  its port number. The field now shows both halves whenever they differ.

## 1.5.5

- Saving a change that belongs to AWS now applies it. The ports, the
  hostname, the health check path, the idle timeout and the tunnel subnet
  are all part of the CloudFormation stack: saving one and stopping there
  left the load balancer checking a port nothing listens on, which reads as
  "the port forward stopped working" with nothing saying a deploy was owed.
  None of those fields touch the tunnel or the key, so it runs without a
  password.
- Fixed: a redeploy could be refused for colliding with its own tunnel.
  Matching a utun device to a profile needs a root-only file, so an
  unprivileged deploy could not recognise itself; it now matches on the
  profile's own addresses as well.

## 1.5.4

- Fixed: 1.5.3 refused correct installs. The privileged stage is handed a
  settings file and nothing else, so it compared that file against the
  built-in default profile name and rejected it — the opposite failure to
  inventing a profile, and just as effective at stopping a deploy. A file is
  now trusted to name its own profile when nobody else has named one, and
  refused only when it contradicts a profile that was asked for explicitly.

## 1.5.3

- Fixed the last way a phantom "default" profile could appear. A stage that
  applies a deployment now refuses a settings file describing a *different*
  VPN profile, instead of ignoring it and falling back to the built-in
  defaults. An app left running from before 1.4.6 still passes one shared
  path for every profile, and the file it points at belongs to whichever
  profile was deployed last — wrong is not the same as absent, and both stop
  the install now.

## 1.5.2

- Setup has a **Delete profile…** button beside Save, for the profile
  selected in the picker. Same two questions as the menu's version, and the
  same result: the AWS stack, the tunnel, the key, the sudoers entry and the
  local record go; the app stays.

## 1.5.1

- Fixed, for good, the phantom "default" VPN profile. The install runs in
  stages that hand each other a file; the stage that applies it fell back to
  built-in defaults when the file was missing, inventing a profile called
  "default" on whatever interface was free and then reporting errors about a
  profile nobody had created. It refuses now, and setup keeps one settings
  path for the whole run instead of recomputing it per stage.
- The suggested name for a new VPN profile is only "default" when it is the
  first one, so the word stops colliding with the AWS profile of that name.

## 1.5.0

- Two different things were both called "profile", and leaving the VPN
  profile's name blank silently made it "default" — which is also what an AWS
  profile is usually called. A deploy with the name empty therefore built a
  VPN profile nobody had asked for, on an interface of its own, and then
  reported errors about `profiles/default` that read as though the AWS
  profile were at fault. Setup asks for a name instead of inventing one, the
  fields are labelled **AWS profile** and **VPN profile name**, and the
  errors say which kind they mean.

## 1.4.9

- Each VPN profile's submenu has **Delete profile…**. It asks twice — once
  to confirm which profile, once to spell out that the AWS stack, the load
  balancer, the certificate, the address and the DNS record go with it — and
  then removes the deployment, the tunnel, the key, the sudoers entry and the
  local record. The app stays; deleting a profile is not uninstalling the
  product, even when it is the last one.
- The background service no longer repeats "no such file" every fifteen
  seconds for a profile whose configuration is not there yet. It says so
  once and waits for the configuration to appear.

## 1.4.8

- Fixed: two saved-but-undeployed profiles were handed the same WireGuard
  interface, because only deployed ones were counted. The second to be
  installed wrote its tunnel configuration over the first's, producing a
  tunnel whose address came from one profile and whose gateway came from
  neither — "the public key of peer :51820 is not a WireGuard key".
- Removed: the installer no longer adopts a private key left by versions
  before per-profile keys. It claimed the new profile's key file, which then
  suppressed the key that had just been registered with the gateway, so the
  tunnel came up with an identity the gateway had never been told about.

## 1.4.7

- Fixed: "utun8 is already on 10.0.0.0/8" when deploying a second profile.
  The tunnel interface was given no netmask, so macOS widened its address to
  the whole class A — the first tunnel appeared to occupy sixteen million
  addresses, and the check that stops a tunnel swallowing a network you are
  on refused every other private range. A tunnel now claims exactly its own
  address, and this product's own interfaces are never mistaken for networks
  to be protected from.
- An existing tunnel keeps the old mask until it is reconnected once.

## 1.4.6

- Fixed: installing a second VPN profile was still refused with "profile X
  already uses the interface wg0". Setup passes a temporary file from one
  stage of the install to the next, and it used the same path for every
  profile — so the file left by the profile deployed last was read as the new
  one's, which then inherited its interface and its endpoint and collided
  with it. The file is per profile now, and a file describing a different
  profile is ignored.

## 1.4.5

- Fixed: a second VPN profile was refused for two things nobody chose. It was
  handed the interface wg0, which the first profile already had, and the same
  stack name, because the name was built from the account and region alone.
  Every profile after the first now gets its own interface and a stack name
  ending in the profile's name; the first keeps
  `mymicrotunnel-<account-id>-<region>`.

## 1.4.4

- Every VPN profile now has its own submenu holding Connect, Test tunnel,
  Open health check and — where the gateway sleeps — Wake gateway. Always a
  submenu, including with one profile, so the menu does not rearrange itself
  when a second appears.
- A profile that has been saved but never deployed is listed too, as "Not
  deployed", instead of being left out. Being invisible read as the save
  having been lost.
- Reconnect at login is no longer in the menu. It belongs with the rest of a
  profile's configuration, in Setup.

## 1.4.3

- Fixed: Save on a new VPN profile appeared to do nothing and cleared the
  form. It had saved: the profile list only showed deployed profiles, so the
  one just saved was not in it, the selection fell back to "New VPN
  Profile…", and that resets the fields. Saved profiles are listed now and
  stay selected.

## 1.4.2

- Fixed: creating a second VPN profile failed with "the tunnel address of the
  gateway is outside the tunnel subnet". The window suggested a subnet of its
  own for the new profile and then refused two addresses you never typed. The
  addresses follow the subnet now — the gateway takes its first usable
  address, this machine the second — and an address deliberately set inside
  the subnet, as a second Mac on one deployment has, is left alone.
- Setup has a **Save** button. It records the profile without deploying
  anything, which is what adjusting a port wants; the stack is untouched
  until Deploy CloudFormation.

## 1.4.1

- With more than one VPN profile, each gets its own submenu holding its
  Connect, Test tunnel, Open health check and Reconnect at login. The top
  level now reads as a list of deployments and their state — ticked when
  connected — instead of a column of identical verbs where the wrong one
  moves somebody else's tunnel. A single profile stays flat: a submenu there
  is a second click for nothing.

## 1.4.0

- Updating now actually replaces what is running. An update swaps the files
  on disk and macOS does not reload a process because its file changed, so
  until now the menu bar app, the background service and the tunnel itself all
  carried on running the previous version — software reporting a version it
  was not running. Installing an update quits the old app, restarts the
  background service and re-raises any tunnel that is up, which costs a second
  of connectivity and is worth it.

## 1.3.2

- New `mymicrotunnel pubkey` prints the public half of a key file. When a
  tunnel sends and never hears back, the question is whether the key on this
  machine is the one the gateway was told about — and until now there was no
  way to answer it without printing a private key into a terminal.

## 1.3.1

- The setup window's button says **Deploy CloudFormation**, because that is
  what it does: build a stack in your own AWS account, over several minutes,
  for money. "Install" undersold it.
- The log is hidden until there is something in it. An empty console filling a
  third of the window before anything has run looks like a fault.
- Setup explains how ports are written, beside the fields that take them:
  local first, published second.

## 1.3.0

- Every published port is now written `local:published`. A bare number
  publishes the port under its own name, as before; `3000:8080` reaches port
  3000 on your Mac and answers as port 8080 on the hostname. The rule covers
  the HTTPS service too — `--port 3000:8443` moves it off 443 — so a service
  already running somewhere no longer has to move to be published elsewhere,
  and two deployments can publish different services on one well-known port.
- Two listeners cannot share a published port and are refused before anything
  is deployed. Two published ports may share a local one, which is a
  reasonable thing to want.

## 1.2.0

- VPN profiles are now something you can see and create. The menu lists every
  deployment by name under **VPN Profiles**, each with its own switch, and
  **New VPN Profile…** creates another — suggesting a name and a tunnel subnet
  that does not overlap the ones you already have.
- Whether a profile reconnects at login is a checkmark in the menu, per
  profile, rather than a decision buried in the installer.
- Uninstalling offers to delete the AWS stack with the profile, instead of
  printing a command and hoping. The consequence is spelled out separately,
  because it is a different one: the hostname stops answering for every Mac
  registered to that deployment.
- The region is a list of the regions your account can actually reach, and the
  stack name fills itself in as `mymicrotunnel-<account-id>-<region>`. A
  mistyped region used to surface much later as a confusing credentials error.
- Setup explains itself on a first run: what to create in AWS, what kind of
  hostname works, and what the one password prompt covers.
- The credentials you type are now used once, to mint a narrower key for the
  app itself, which is kept in your login Keychain. The app no longer runs as
  you, and its own key cannot create more keys.
- Fixed: the idle timeout switched the gateway off after about ninety seconds
  whatever it was set to, because a load balancer with no traffic publishes no
  metric at all rather than a zero.
- Fixed: a tunnel whose subnet overlaps a network your Mac is already on is
  refused, at install and at every connect, instead of quietly taking that
  network away.
- Fixed: the privileged step looked for the newly generated key in root's
  temporary directory, wrote a configuration naming a key it never installed,
  and left a tunnel that came up and could not load a private key.
- Fixed: the setup window could grow taller than the screen, putting Install
  out of reach. It scrolls, and the button no longer moves.

## 1.1.0

- **The product is now called MyMicroTunnel.** The app, the command and the
  paths it installs are renamed with it: the command is `mymicrotunnel`, the app
  is `MyMicroTunnel.app`, and settings move to
  `~/Library/Application Support/MyMicroTunnel/profiles/`.
- Up to ten extra TCP ports can be published on the same hostname, forwarded
  as plain TCP alongside the HTTPS service — a database, an SSH daemon,
  anything. `--tcp-ports 5432,6379`. They are open to the internet, so put
  authentication on them.
- One Mac can now hold several deployments at once. Each VPN profile has its own
  stack, tunnel subnet, WireGuard interface, published ports and switch in the
  menu: `mymicrotunnel install --vpn-profile lab --vpn-cidr 10.110.0.0/24`.
- The tunnel subnet is yours to choose per profile, with `--vpn-cidr`, and two
  profiles that would collide are refused before anything is deployed.
- New: let the gateway sleep. With `--idle-timeout 30`, the EC2 instance is
  switched off after thirty minutes with no traffic through the load balancer,
  and comes back when you press Connect — about two minutes — or when the
  background service notices a tunnel that should be up has gone quiet. The
  hostname, the certificate and the address all survive; most of the bill does
  not.
- Waking runs through an IAM role the stack creates for that one purpose. It can
  ask this deployment's Auto Scaling group for an instance and nothing else, so
  the credential your Mac uses daily is not one that could delete the
  deployment.
- The hostname's DNS record is now written by the installer rather than by
  CloudFormation, so deploying onto a name that already exists in your zone
  repoints it instead of rolling the whole stack back. The zone itself is never
  created or touched. The hostname must have three labels — `updates.example.com`,
  not `example.com`, which is the apex your website usually sits on.
- A tunnel is refused, at install and every time it is raised, when its subnet
  overlaps a network this Mac is already on or another profile's subnet. The
  old behaviour was to come up and quietly take the LAN away: no printer, no
  router, no obvious cause.
- The default stack name is now `mymicrotunnel-<account-id>-<region>`, so a second
  deployment in an account no longer has to be named by hand to avoid
  overwriting the first.

## 1.0.3

- The menu bar can now remove MyMicroTunnel from this Mac. It explains what it does
  not touch — your private key, and the AWS stack, which keeps costing money
  until you delete it from a terminal.
- New `mymicrotunnel diagnose` collects everything a support conversation
  would otherwise ask for over three rounds of email: versions, tunnel state and
  handshake age, whether the published service answers on loopback *and* on the
  tunnel address, stack outputs, peer registry, target health, and the
  supervisor's last decisions. Safe to paste — the AWS account is masked and no
  secret is read.
- Installing no longer starts with pasting IAM JSON into a console. A
  launch-stack link creates the deploy identity in your own account.
- Fixed: a configuration file written by an older version was discarded whole
  rather than read partially, so the app silently fell back to compiled
  defaults after an update — the wrong interface at the wrong address. This
  would have affected every future update; it affects none now.

## 1.0.2

- Fixed: the release archive held the package one directory down from where
  Sparkle looks for it. Updates downloaded and verified correctly and then
  failed to install. 1.0.1 is affected and superseded by this release.

## 1.0.1

- Superseded by 1.0.2. Its update archive is malformed and will not install.

## 1.0.0

- First release.
- Publishes a service running on your Mac at a public HTTPS hostname, through an
  AWS Network Load Balancer and a WireGuard tunnel in your own AWS account.
- The menu bar switch raises and drops the tunnel; the hostname answers only
  while it is up.
- Carries its own WireGuard, so there is nothing to install first — no Homebrew,
  no AWS CLI, no Python.
- Optionally restores the tunnel to its last state after a reboot, and re-pins it
  when your home IP address changes.
- Several Macs can serve one hostname; adding one does not interrupt the others.
- The gateway is an Auto Scaling group of one, so a failed instance is replaced
  and keeps the same address and the same WireGuard identity.
