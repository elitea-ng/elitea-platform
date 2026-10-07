# Managed app configuration (MDM AppConfig)

An MDM (Jamf, Intune, Kandji, Workspace ONE, ...) can push a configuration
dictionary to Agent Zefir on iOS. The app reads it from the managed
UserDefaults key `com.apple.configuration.managed` at launch and again
whenever the MDM changes or removes it while the app runs
(`src/security/managedConfig.ts`). Removing the AppConfig, or unenrolling
the device, drops the lock floor and the suggested address at once, not at
the next launch.

| Key | Type | Effect |
|---|---|---|
| `server_url` | string | Pre-fills the workspace address on the connect screen ("Suggested by your organization"). The user still confirms the workspace, and the usual address checks apply (https only, no credentials in the URL). |
| `require_device_lock` | boolean | `true` requires Face ID / Touch ID / passcode at launch, whatever the workspace policy says. `false` changes nothing. |
| `idle_lock_seconds` | integer, 1 to 86400 | With a device lock, relock after this many seconds in the background when it is shorter than the workspace's (or the workspace sets 0, "never relock"). `0` changes nothing. |
| `allow_screenshots` | boolean | `false` blocks screenshots and blurs the app switcher snapshot where the device allows it. `true` changes nothing. |

The lock keys are a **floor**: they only ever tighten the workspace's client
policy, never loosen it. Settings says when the device management made the
lock stricter. A key of the wrong type is ignored. The configuration belongs
to the device, not to a workspace: logging out of a workspace does not
remove it.

Example (Jamf / Intune "app configuration" plist):

```xml
<dict>
  <key>server_url</key><string>https://ai.example.com</string>
  <key>require_device_lock</key><true/>
  <key>idle_lock_seconds</key><integer>60</integer>
  <key>allow_screenshots</key><false/>
</dict>
```

Android managed configurations (RestrictionsManager) are not read yet.
