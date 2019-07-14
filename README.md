# amicitizen_serv
service to check the validity by number of Russian passport against guvm.mvd.ru database

Serves on localhost and listens on port set in the config file. Just POST a passport number to `/` and you'll get an answer whether it's valid.
Note, that it takes a time to download an enormously big csv from the server. Also a memory footprint is huge during the download as amacitizen_serv unbzips the file on the fly. This caveat yet to be resolved. 

You could ask amicitizen to check for updates by pinging `/update`, however it checks for newer version of a remote file every n hours as specified in the config. 
