# Declarative hardware inventory

HardwareReport accepts bounded optional encoders, displays, codec profiles and
acceleration declarations. Existing clients without these fields remain valid.
Display IDs and mode IDs must be unique within their scope; active modes must match
the dimensions/refresh of the selected mode. Profiles must reference an advertised
MIME. Encoders cannot advertise decoder probe candidates. Inventory never creates
functional probe results, selects hardware acceleration or enables an OEM backend.

The diagnostic export includes bounded codec/display records and counts, but no
display names. The existing 64-KiB hardware request budget still applies. Both Full
and Edge use this implementation. Host tests cover legacy defaults, valid native
inventory, malformed/contradictory records, export and absence of generated PASS
results. Physical/OEM and broader multimedia acceptance remain deferred.
