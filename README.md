# amicitizen_serv
service to check the validity by number of Russian passport against guvm.mvd.ru database

Serves on localhost and listens on port set in the config file. Just POST a passport number to `/` and you'll get an answer whether it's valid.
Note, that it takes a time to download the big csv from the server. Some steps made to keep the memory footprint as small as possible, however it still is huge (tops around ~500 Mb) during the download as amacitizen_serv unbzips the file on the fly.

You could ask amicitizen to check for updates by pinging `/update`, however it checks for newer version of a remote file every n hours as specified in the config. 
