DROP TRIGGER IF EXISTS document_events_outbox_notify ON document_events_outbox;
DROP FUNCTION IF EXISTS tw_outbox_notify();
DROP TABLE IF EXISTS document_events_outbox_dead;
DROP TABLE IF EXISTS document_events_outbox;
