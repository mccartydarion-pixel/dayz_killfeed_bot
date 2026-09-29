package database

// CASEBuildEvidenceSQL keeps parsed ADM build-action fields on the existing
// source-addressed observation. Empty fields never establish a base intrusion.
// The parser provenance remains attached to the line and the guild/server scope.
const CASEBuildEvidenceSQL = `
ALTER TABLE case_evidence_events
 ADD COLUMN IF NOT EXISTS build_action TEXT NOT NULL DEFAULT '',
 ADD COLUMN IF NOT EXISTS build_object TEXT NOT NULL DEFAULT '',
 ADD COLUMN IF NOT EXISTS build_target TEXT NOT NULL DEFAULT '',
 ADD COLUMN IF NOT EXISTS build_tool TEXT NOT NULL DEFAULT '';
ALTER TABLE case_evidence_events
 ADD CONSTRAINT case_build_action_valid CHECK (
   build_action IN ('','Placed','Built','Dismantled')
   AND char_length(build_object) <= 64
   AND char_length(build_target) <= 64
   AND char_length(build_tool) <= 64
   AND ((event_type='BUILD_ACTION') = (build_action <> ''))
   AND ((build_action = '') = (build_object = ''))
 ) NOT VALID;
`;
